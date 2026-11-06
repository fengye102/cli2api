package custom

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

// ChatStream posts the internal chat request to the relay's Chat Completions
// endpoint and hands the upstream SSE body back untouched — the gateway already
// speaks this dialect, so there is nothing to rewrite.
//
// /v1/responses is deliberately never used: compatible relays generally do not
// implement it, which is exactly the trap sub2api documents in its
// openai_compat package.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	resp, err := c.doChat(ctx, accountID, req)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, providers.ResolvedChat{}, newProviderError(resp.StatusCode, string(body))
	}
	return resp, providers.ResolvedChat{}, nil
}

// ChatNonStream still asks the relay to stream, then assembles the answer. One
// path keeps usage accounting and tool-call assembly in a single place, and
// every OpenAI-compatible relay streams.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	resp, err := c.doChat(ctx, accountID, req)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return providers.ChatOutcome{}, newProviderError(resp.StatusCode, string(body))
	}
	return collectChatSSE(resp.Body, req.Model)
}

func (c *Client) doChat(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	model := upstreamModel(req.Model, credential)
	if model == "" {
		return nil, &providers.Error{Kind: "invalid_request", Status: http.StatusBadRequest, Message: "model 不能为空"}
	}
	payload, err := buildChatBody(req, model)
	if err != nil {
		return nil, err
	}
	base, err := credential.ChatBase()
	if err != nil {
		return nil, &providers.Error{Kind: "invalid_request", Status: http.StatusBadRequest, Message: err.Error()}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+ChatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, &providers.Error{Kind: "invalid_request", Status: http.StatusBadRequest, Message: err.Error()}
	}
	httpReq.Header.Set("Authorization", "Bearer "+credential.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, &providers.Error{
			Kind:    "unavailable",
			Message: "自定义上游不可达：" + err.Error(),
			// Keep the chain so a dial/TLS failure is recognised by shape and
			// does not park a healthy account.
			Err: err,
		}
	}
	return resp, nil
}

// buildChatBody serialises the internal request for a generic OpenAI-compatible
// endpoint: model rewritten, stream forced, and the `developer` role folded into
// `system`.
//
// The role swap is the one place this adapter touches message content. OpenAI
// accepts `developer`, but the upstreams relays commonly front (DeepSeek, GLM,
// Qwen, Kimi) reject it as an unknown role, so passing it through would turn a
// working request into a 400 for no benefit.
func buildChatBody(req translate.ChatRequest, model string) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return nil, err
	}
	obj["model"] = model
	obj["stream"] = true
	foldDeveloperRoles(obj)
	return json.Marshal(obj)
}

func foldDeveloperRoles(obj map[string]any) {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); strings.EqualFold(role, "developer") {
			message["role"] = "system"
		}
	}
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

type sseAggregate struct {
	content      strings.Builder
	messageText  strings.Builder
	reasoning    strings.Builder
	messageThink strings.Builder
	tools        map[int]*toolCall
	order        []int
	usage        chatUsage
	finish       string
}

// collectChatSSE assembles a streamed OpenAI answer into the non-stream shape.
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
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Message struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"message"`
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
			// Some relays answer a stream request with one full message
			// instead of deltas. The two shapes are collected separately and
			// one is chosen at the end — appending both would duplicate the
			// answer whenever a relay sends deltas and then a final message.
			agg.content.WriteString(choice.Delta.Content)
			agg.messageText.WriteString(choice.Message.Content)
			agg.reasoning.WriteString(firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Reasoning))
			agg.messageThink.WriteString(firstNonEmpty(choice.Message.ReasoningContent, choice.Message.Reasoning))
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
	content, reasoning := agg.content.String(), agg.reasoning.String()
	if content == "" {
		content = agg.messageText.String()
	}
	if reasoning == "" {
		reasoning = agg.messageThink.String()
	}
	outcome := providers.ChatOutcome{
		Model:            model,
		Content:          content,
		Reasoning:        reasoning,
		FinishReason:     agg.finish,
		PromptTokens:     agg.usage.PromptTokens,
		CompletionTokens: agg.usage.CompletionTokens,
		UsageSource:      "upstream",
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
