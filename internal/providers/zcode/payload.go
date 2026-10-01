package zcode

import (
	"encoding/json"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

// anthropicBody builds the Anthropic Messages request body from the shared
// chat-completions shape. The upstream is /v1/messages on the Anthropic
// protocol; stream is always forced true so non-stream callers can collect.
//
// System prompts are pulled out of messages into the top-level "system"
// field; tool results travel as a user message carrying tool_result blocks;
// assistant tool_calls become tool_use blocks. Reasoning is catalog-driven
// via output_config.effort (GLM-5.x on Coding Plan), never invented.
//
// maxTokensCap is the catalog MaxOutput for the resolved model (0 = unknown);
// it is used only to default max_tokens when the client omitted one. Client
// values pass through unchanged, matching the cross-provider behavior.
func anthropicBody(req translate.ChatRequest, reasoningLevel string, maxTokensCap int) map[string]any {
	body := map[string]any{
		"model":      req.Model,
		"stream":     true,
		"max_tokens": resolveMaxTokens(req, maxTokensCap),
	}
	if temperature, ok := rawNumber(req.Temperature); ok {
		body["temperature"] = temperature
	}
	if topP, ok := rawNumber(req.TopP); ok {
		body["top_p"] = topP
	}
	if stop := stopSequences(req.Stop); len(stop) > 0 {
		body["stop_sequences"] = stop
	}
	system, messages := splitSystem(req.Messages)
	if system != "" {
		body["system"] = system
	}
	body["messages"] = messages
	if tools := anthropicTools(req.Tools); len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := anthropicToolChoice(req.ToolChoice); choice != nil {
		body["tool_choice"] = choice
	}
	if reasoningLevel != "" && reasoningLevel != "none" {
		// output_config.effort is the ZCode plan gateway shape for GLM
		// reasoning tiers; do not invent levels a model does not declare.
		body["output_config"] = map[string]any{"effort": reasoningLevel}
	}
	return body
}

// defaultMaxTokens is the last-resort max_tokens when neither the client nor
// the catalog declare one. Anthropic Messages rejects a missing/zero field,
// so the body must always carry a positive value.
const defaultMaxTokens = 8192

// resolveMaxTokens returns the client's max_tokens (or max_completion_tokens)
// when present and positive; otherwise the catalog MaxOutput cap; otherwise
// defaultMaxTokens. Client values pass through unchanged — we do not cap at
// the model's declared max output, matching the other providers.
func resolveMaxTokens(req translate.ChatRequest, catalogMaxOutput int) int {
	for _, raw := range []json.RawMessage{req.MaxTokens, req.MaxCompletionTokens} {
		if n, ok := rawInt(raw); ok && n > 0 {
			return n
		}
	}
	if catalogMaxOutput > 0 {
		return catalogMaxOutput
	}
	return defaultMaxTokens
}

func rawInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int(f), true
	}
	return 0, false
}

func rawNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	return f, true
}

func stopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		out := make([]string, 0, len(list))
		for _, s := range list {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil && strings.TrimSpace(single) != "" {
		return []string{strings.TrimSpace(single)}
	}
	return nil
}

// splitSystem separates system/developer messages from the conversational
// stream. Anthropic Messages requires system as a top-level field; multiple
// system entries are joined with a blank line.
func splitSystem(input []translate.ChatMessage) (string, []map[string]any) {
	var system []string
	out := make([]map[string]any, 0, len(input))
	// Buffer assistant-with-tool_calls so we can fuse the immediately
	// following tool-result messages into the next user message.
	pendingTools := map[string]bool{}
	flushTools := func() { pendingTools = map[string]bool{} }
	for _, m := range input {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			if text := translate.ContentToString(m.Content); strings.TrimSpace(text) != "" {
				system = append(system, text)
			}
		case "tool":
			// A tool result becomes a user message with tool_result blocks.
			// Consecutive tool results are merged into a single user message.
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     translate.ContentToString(m.Content),
			}
			if n := len(out); n > 0 && out[n-1]["role"] == "user" && pendingTools[m.ToolCallID] {
				if arr, ok := out[n-1]["content"].([]any); ok {
					out[n-1]["content"] = append(arr, block)
					continue
				}
			}
			out = append(out, map[string]any{"role": "user", "content": []any{block}})
		case "assistant":
			flushTools()
			msg := map[string]any{"role": "assistant"}
			blocks := assistantBlocks(m)
			if len(blocks) == 0 {
				continue
			}
			msg["content"] = blocks
			for _, b := range blocks {
				if tb, ok := b.(map[string]any); ok && tb["type"] == "tool_use" {
					if id, _ := tb["id"].(string); id != "" {
						pendingTools[id] = true
					}
				}
			}
			out = append(out, msg)
		default:
			flushTools()
			msg := map[string]any{"role": "user"}
			if content := anthropicUserContent(m.Content); len(content) > 0 {
				msg["content"] = content
			} else if text := translate.ContentToString(m.Content); strings.TrimSpace(text) != "" {
				msg["content"] = text
			} else {
				continue
			}
			out = append(out, msg)
		}
	}
	return strings.Join(system, "\n\n"), out
}

// assistantBlocks converts an assistant turn into Anthropic content blocks:
// plain text becomes a text block, tool_calls become tool_use blocks.
func assistantBlocks(m translate.ChatMessage) []any {
	blocks := make([]any, 0)
	if text := strings.TrimSpace(translate.ContentToString(m.Content)); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	if len(m.ToolCalls) == 0 {
		return blocks
	}
	var calls []map[string]any
	if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
		return blocks
	}
	for _, call := range calls {
		id, _ := call["id"].(string)
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		argsRaw, _ := fn["arguments"].(string)
		if id == "" || name == "" {
			continue
		}
		// The Anthropic tool_use block takes a decoded input object; malformed
		// JSON arguments are dropped rather than sent as a string.
		var input any
		if argsRaw == "" {
			input = map[string]any{}
		} else if err := json.Unmarshal([]byte(argsRaw), &input); err != nil {
			input = map[string]any{}
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": input,
		})
	}
	return blocks
}

// anthropicUserContent translates an OpenAI multi-part user message into
// Anthropic content blocks (text + image_url). Plain string content returns
// nil so the caller can keep it as a string field.
func anthropicUserContent(content any) []any {
	list, ok := content.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	blocks := make([]any, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "text":
			if text, _ := m["text"].(string); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		case "image_url":
			if block := anthropicImageBlock(m); block != nil {
				blocks = append(blocks, block)
			}
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	return blocks
}

// anthropicImageBlock converts an OpenAI image_url part into the Anthropic
// base64 image source shape. Data URLs are decoded; http(s) URLs are skipped
// because the upstream only accepts base64 sources for the coding plan.
func anthropicImageBlock(m map[string]any) map[string]any {
	raw, ok := m["image_url"].(map[string]any)
	if !ok {
		if s, isStr := m["image_url"].(string); isStr {
			raw = map[string]any{"url": s}
		} else {
			return nil
		}
	}
	url, _ := raw["url"].(string)
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	// data:<mime>;base64,<payload>
	rest := strings.TrimPrefix(url, "data:")
	semicolon := strings.Index(rest, ";")
	comma := strings.Index(rest, ",")
	if semicolon < 0 || comma < 0 || comma < semicolon {
		return nil
	}
	mime := rest[:semicolon]
	data := rest[comma+1:]
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mime,
			"data":       data,
		},
	}
}

// anthropicTools maps OpenAI function tools to Anthropic tool definitions.
// Namespace-wrapped tools are flattened first via the shared normalize helper.
func anthropicTools(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	normalized, err := translate.NormalizeOpenAITools(raw)
	if err != nil || len(normalized) == 0 {
		return nil
	}
	var flat []map[string]any
	if err := json.Unmarshal(normalized, &flat); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(flat))
	for _, item := range flat {
		fn, _ := item["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		def := map[string]any{"name": name}
		if desc, _ := fn["description"].(string); desc != "" {
			def["description"] = desc
		}
		if params, ok := fn["parameters"]; ok && params != nil {
			def["input_schema"] = params
		} else {
			def["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, def)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// anthropicToolChoice converts OpenAI tool_choice to the Anthropic shape:
// "auto"/"required" pass through as {type:auto|any}; a specific function
// becomes {type:tool,name:...}. "none" drops tools entirely upstream.
func anthropicToolChoice(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		}
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	typ, _ := obj["type"].(string)
	switch strings.ToLower(typ) {
	case "auto":
		return map[string]any{"type": "auto"}
	case "required":
		return map[string]any{"type": "any"}
	case "function":
		name := ""
		if fn, ok := obj["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
		if name == "" {
			return nil
		}
		return map[string]any{"type": "tool", "name": name}
	}
	return nil
}
