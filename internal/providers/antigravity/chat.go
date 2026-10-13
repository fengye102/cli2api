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

// Chat execution. The adapter has two upstream planes and picks the one the
// account's credential belongs to:
//
//	key mode   — Gemini's OpenAI-compatibility layer; the upstream SSE body is
//	             already chat-shaped and is relayed unchanged.
//	oauth mode — Cloud Code Assist (v1internal); the upstream speaks a Gemini
//	             envelope, so the body is translated into chat SSE here.

// ChatStream posts the internal chat request and returns a chat-shaped SSE body.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	credential, err := c.freshCredential(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if !credential.IsOAuth() {
		resp, resolved, err := c.doChat(ctx, accountID, credential, req)
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
	return c.doOAuthChat(ctx, accountID, credential, req)
}

// ChatNonStream forces a stream, then collects it. One path keeps usage and
// tool-call assembly in one place, matching the sibling providers.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	resp, resolved, err := c.ChatStream(ctx, accountID, req)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	outcome, err := collectChatSSE(resp.Body, req.Model)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	outcome.ReasoningLevel = resolved.ReasoningLevel
	return outcome, nil
}

// doChat posts to the OpenAI-compatibility layer (key mode) and relays its SSE
// body unchanged.
func (c *Client) doChat(ctx context.Context, accountID string, credential Credential, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
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

// doOAuthChat posts a v1internal envelope (oauth mode) and rewrites the Gemini
// SSE body into chat SSE.
func (c *Client) doOAuthChat(ctx context.Context, accountID string, credential Credential, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	model := resolveModel(req.Model)
	if model == "" {
		return nil, providers.ResolvedChat{}, newProviderError(400, "antigravity model is required")
	}
	payload, err := chatRequestBody(req, model, credential.ProjectID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	var lastErr error
	// The three environments are tried in order: 429/5xx/transport failures move
	// to the next one, a 4xx is this request's own problem and does not.
	for _, base := range v1BaseURLs {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, streamGenerateURL(base), bytes.NewReader(payload))
		if err != nil {
			return nil, providers.ResolvedChat{}, err
		}
		for key, value := range contentHeaders(credential.AccessToken) {
			httpReq.Header.Set(key, value)
		}
		httpReq.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = &providers.Error{Kind: "unavailable", Status: 502, Message: "Antigravity 请求失败：" + err.Error()}
			continue
		}
		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			providerErr := newProviderError(resp.StatusCode, string(body))
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
				lastErr = providerErr
				continue
			}
			return nil, providers.ResolvedChat{}, providerErr
		}
		return rewriteGeminiStream(resp, model), providers.ResolvedChat{}, nil
	}
	if lastErr == nil {
		lastErr = &providers.Error{Kind: "unavailable", Status: 502, Message: "Antigravity 的所有环境均不可用"}
	}
	return nil, providers.ResolvedChat{}, lastErr
}

// rewriteGeminiStream wraps the upstream body so callers see chat SSE.
func rewriteGeminiStream(resp *http.Response, model string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		defer pw.Close()
		if err := translateGeminiStream(resp.Body, pw, model); err != nil {
			_ = pw.CloseWithError(err)
		}
	}()
	out := *resp
	out.Body = pr
	out.ContentLength = -1
	header := resp.Header.Clone()
	header.Del("Content-Length")
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	out.Header = header
	return &out
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
