package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// ChatStream posts the internal chat request to the Loomy gateway and returns
// the upstream SSE body unchanged. A dead session is reported by the gateway as
// HTTP 401 *or* as a 200 with the 100002 business envelope, so both shapes are
// handled by chatWithRelogin: the adapter logs in again once and retries.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	return c.chatWithRelogin(ctx, accountID, req)
}

// ChatNonStream forces a stream and collects it, because the collector is the
// only parser that tolerates every chunk shape the gateway emits.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	resp, resolved, err := c.chatWithRelogin(ctx, accountID, req)
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

// chatWithRelogin runs one chat attempt and, if the session turned out to be
// dead, logs in again and retries exactly once.
func (c *Client) chatWithRelogin(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	resp, resolved, err := c.doChat(ctx, accountID, req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		err = &providers.Error{Kind: accounts.KindAuth, Status: http.StatusUnauthorized, Message: "loomy session 已失效"}
	}
	if err == nil {
		resp, err = requireSSE(resp)
	}
	if err != nil {
		if !isAuthError(err) {
			return nil, providers.ResolvedChat{}, err
		}
		if _, reloginErr := c.relogin(ctx, accountID); reloginErr != nil {
			return nil, providers.ResolvedChat{}, reloginErr
		}
		resp, resolved, err = c.doChat(ctx, accountID, req)
		if err != nil {
			return nil, providers.ResolvedChat{}, err
		}
		if resp.StatusCode == http.StatusUnauthorized {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, providers.ResolvedChat{}, newProviderError(resp.StatusCode, string(body))
		}
		if resp, err = requireSSE(resp); err != nil {
			return nil, providers.ResolvedChat{}, err
		}
	}
	return resp, resolved, nil
}

// requireSSE rejects a successful-looking response that is actually a JSON
// business error, and leaves a real stream untouched.
func requireSSE(resp *http.Response) (*http.Response, error) {
	if err := peekJSONError(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func isAuthError(err error) bool {
	providerErr, ok := err.(*providers.Error)
	return ok && providerErr.Kind == accounts.KindAuth
}

func (c *Client) doChat(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	model := resolveModel(req.Model)
	if model == "" {
		return nil, providers.ResolvedChat{}, newProviderError(http.StatusBadRequest, "loomy model is required")
	}
	// A generation model has no chat plane: forwarding it upstream only earns
	// a 404 "该模型暂未开放". Point the caller at the endpoint that does serve
	// it instead of spending a round trip.
	if (catalogEntry{ID: model}).imageModel() {
		return nil, providers.ResolvedChat{}, &providers.Error{
			Kind:    accounts.KindInvalidRequest,
			Status:  http.StatusBadRequest,
			Message: publicModel(model) + " 是图像生成模型，请改用 POST /v1/images/generations（body 与 OpenAI 一致：model + prompt）",
		}
	}
	payload, err := buildChatBody(req, model)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	endpoint := strings.TrimRight(c.modelBase, "/") + chatPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	for key, value := range streamHeaders(cred.Session) {
		httpReq.Header.Set(key, value)
	}
	client, err := c.modelClient(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, providers.ResolvedChat{}, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Message: "loomy 上游不可达：" + err.Error(),
		}
	}
	return resp, providers.ResolvedChat{}, nil
}

// collectChatSSE folds an OpenAI SSE stream into one outcome.
type sseAggregate struct {
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*toolCall
	order     []int
	usage     chatUsage
	finish    string
	points    *float64
}

type toolCall struct {
	id   string
	name string
	args strings.Builder
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// PointsConsumed is Loomy's own accounting: how many credits the request
	// spent. The gateway reports it inside `usage`.
	PointsConsumed *float64 `json:"points_consumed"`
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
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 || chunk.Usage.PointsConsumed != nil {
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
		Credits:          agg.usage.PointsConsumed,
		UsageSource:      "loomy",
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
