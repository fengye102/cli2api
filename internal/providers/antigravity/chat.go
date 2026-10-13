package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// ChatStream posts the internal chat request to Gemini's OpenAI-compat
// /chat/completions and returns the upstream SSE body unchanged. The gateway
// already speaks this dialect, so there is no stream rewrite.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	resp, resolved, err := c.doChat(ctx, accountID, req)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, providers.ResolvedChat{}, newProviderError(resp.StatusCode, string(body))
	}
	return resp, resolved, nil
}

// ChatNonStream forces a stream, then collects it. One path keeps usage and
// tool-call assembly in one place, matching the sibling providers.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	resp, resolved, err := c.doChat(ctx, accountID, req)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return providers.ChatOutcome{}, newProviderError(resp.StatusCode, string(body))
	}
	outcome, err := collectChatSSE(resp.Body, req.Model)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	outcome.ReasoningLevel = resolved.ReasoningLevel
	return outcome, nil
}

func (c *Client) doChat(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	model := resolveModel(req.Model)
	if model == "" {
		return nil, providers.ResolvedChat{}, newProviderError(400, "antigravity model is required")
	}
	level, _ := c.resolveLevel(ctx, accountID, model, requestedLevel(req))
	payload, err := buildChatBody(req, model, level)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	base, err := credential.ChatBase()
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(base, PathChat), bytes.NewReader(payload))
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+credential.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	return resp, providers.ResolvedChat{ReasoningLevel: level}, nil
}

func resolveModel(model string) string {
	clean := strings.TrimSpace(model)
	for _, prefix := range []string{"antigravity/", "gemini/"} {
		clean = strings.TrimPrefix(clean, prefix)
	}
	return strings.TrimSpace(clean)
}

func requestedLevel(req translate.ChatRequest) string {
	if len(req.ReasoningEffort) == 0 {
		return ""
	}
	var value any
	if json.Unmarshal(req.ReasoningEffort, &value) != nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return providers.NormalizeReasoningLevel(typed)
	case map[string]any:
		for _, key := range []string{"effort", "level", "type"} {
			if text, ok := typed[key].(string); ok {
				if level := providers.NormalizeReasoningLevel(text); level != "" {
					return level
				}
			}
		}
	}
	return ""
}

// resolveLevel clamps the requested reasoning level against the model's
// declared options, falling back to the console default when nothing is
// requested. Only gemini-* models carry options in this adapter.
func (c *Client) resolveLevel(ctx context.Context, accountID, reqModel, requested string) (string, providers.ModelCapabilities) {
	caps := reasoningCaps(reqModel)
	if info, ok := c.lookupModel(accountID, reqModel); ok && len(info.Capabilities.ReasoningOptions) > 0 {
		caps = info.Capabilities
	}
	level := providers.ResolveReasoningLevel(requested, caps)
	if level == "" {
		level = c.storedReasoning(ctx, reqModel)
		level = providers.ResolveReasoningLevel(level, caps)
	}
	return level, caps
}

type sseAggregate struct {
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*toolCall
	order     []int
	usage     chatUsage
	finish    string
}

type toolCall struct {
	id   string
	name string
	args strings.Builder
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func collectChatSSE(body io.Reader, model string) (providers.ChatOutcome, error) {
	agg := &sseAggregate{tools: map[int]*toolCall{}, finish: "stop"}
	raw, err := io.ReadAll(io.LimitReader(body, 16<<20))
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage chatUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			agg.usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			agg.content.WriteString(choice.Delta.Content)
			agg.reasoning.WriteString(choice.Delta.ReasoningContent)
			if choice.FinishReason != "" {
				agg.finish = choice.FinishReason
			}
			for _, call := range choice.Delta.ToolCalls {
				state := agg.tools[call.Index]
				if state == nil {
					state = &toolCall{}
					agg.tools[call.Index] = state
					agg.order = append(agg.order, call.Index)
				}
				if call.ID != "" {
					state.id = call.ID
				}
				if call.Function.Name != "" {
					state.name = call.Function.Name
				}
				state.args.WriteString(call.Function.Arguments)
			}
		}
	}
	outcome := providers.ChatOutcome{
		Model:            model,
		Content:          agg.content.String(),
		Reasoning:        agg.reasoning.String(),
		FinishReason:     agg.finish,
		PromptTokens:     agg.usage.PromptTokens,
		CompletionTokens: agg.usage.CompletionTokens,
	}
	if len(agg.order) > 0 {
		calls := make([]map[string]any, 0, len(agg.order))
		for _, index := range agg.order {
			state := agg.tools[index]
			calls = append(calls, map[string]any{
				"id":   state.id,
				"type": "function",
				"function": map[string]any{
					"name":      state.name,
					"arguments": state.args.String(),
				},
			})
		}
		encoded, err := json.Marshal(calls)
		if err != nil {
			return providers.ChatOutcome{}, err
		}
		outcome.ToolCalls = encoded
	}
	return outcome, nil
}
