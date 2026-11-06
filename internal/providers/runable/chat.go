package runable

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// The chat endpoint takes one prompt string, not a message array: the web client
// has already flattened the conversation before it posts. Roles are therefore
// carried in the transcript itself — the caller's system prompt goes in verbatim
// at the top, assistant turns are prefixed with [assistant] and tool results with
// [tool] — which is what the reference gateway does and what the model expects.
//
// Tools are NOT sent: this endpoint has no tool channel (the vendor exposes
// agent mode on a different endpoint that this adapter does not use), so a
// request carrying tools gets a plain answer. The catalog reports Tools=false so
// a client can see that before it tries.

// messageText flattens one message's content into text: a plain string, an array
// of OpenAI content parts, or anything else serialised as JSON so nothing is
// silently lost.
func messageText(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			switch part := item.(type) {
			case string:
				parts = append(parts, part)
			case map[string]any:
				if text, ok := part["text"].(string); ok && text != "" {
					parts = append(parts, text)
					continue
				}
				if kind, _ := part["type"].(string); kind == "image_url" {
					// The chat endpoint takes text only; name the omission
					// instead of pretending the image was seen.
					parts = append(parts, "[image omitted: this endpoint is text only]")
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(raw)
	}
}

// buildPrompt folds the conversation into the single string the endpoint expects.
func buildPrompt(messages []translate.ChatMessage) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		text := strings.TrimSpace(messageText(message.Content))
		if text == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "developer":
			// Verbatim: the agent's own instructions must reach the model
			// unchanged, which is why accounts of this provider keep their
			// system prompt (see accounts.DefaultDropSystemPrompt).
			parts = append(parts, text)
		case "assistant":
			parts = append(parts, "[assistant]: "+text)
		case "tool", "function":
			parts = append(parts, "[tool]: "+text)
		default:
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// randomUUID is the id the endpoint expects on the message envelope.
func randomUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// doChat posts one turn and returns the raw stream.
func (c *Client) doChat(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, &providers.Error{Kind: accounts.KindInvalidRequest, Status: http.StatusBadRequest,
			Message: "model 不能为空"}
	}
	prompt := buildPrompt(req.Messages)
	if strings.TrimSpace(prompt) == "" {
		return nil, &providers.Error{Kind: accounts.KindInvalidRequest, Status: http.StatusBadRequest,
			Message: "请求里没有可发送的内容"}
	}
	payload, err := json.Marshal(map[string]any{
		"id":      randomUUID(),
		"trigger": "submit-message",
		"modelId": model,
		"message": map[string]any{
			"id":   randomUUID(),
			"role": "user",
			"parts": []any{
				map[string]any{"type": "text", "text": prompt},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin()+ChatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	applyHeaders(httpReq.Header, cred)
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
			Message: "runable 上游不可达：" + err.Error(),
			// Keep the chain so a dial/TLS failure is recognised by shape.
			Err: err,
		}
	}
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, newProviderError(resp.StatusCode, string(raw))
	}
	return resp, nil
}

// ChatStream relays the AI SDK stream as OpenAI chunks.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	resp, err := c.doChat(ctx, accountID, req)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	return rewriteStream(resp, req.Model), providers.ResolvedChat{}, nil
}

// ChatNonStream asks for a stream and assembles it: the endpoint has no
// non-stream mode of its own.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	resp, err := c.doChat(ctx, accountID, req)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	content, reasoning, counts, finish, err := collectStream(resp.Body)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	return providers.ChatOutcome{
		Model:            req.Model,
		Content:          content,
		Reasoning:        reasoning,
		FinishReason:     finish,
		PromptTokens:     counts.Input,
		CompletionTokens: counts.Output,
		UsageSource:      "upstream",
	}, nil
}
