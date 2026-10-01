package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// chatURL picks the upstream /v1/messages URL for the credential: an
// explicit base_url override wins, then the region's ChatBase, then the
// zai default. The credential carries only the base; the path is fixed.
func chatURL(cred Credential) string {
	base := strings.TrimRight(strings.TrimSpace(cred.BaseURL), "/")
	if base != "" {
		return base + "/v1/messages"
	}
	if region, ok := providers.ZCode.Region(cred.Provider); ok && strings.TrimSpace(region.ChatBase) != "" {
		return strings.TrimRight(region.ChatBase, "/") + "/v1/messages"
	}
	region, _ := providers.ZCode.Region(providers.ZCode.DefaultRegion)
	return strings.TrimRight(region.ChatBase, "/") + "/v1/messages"
}

// chatAuthHeaders applies the credential's auth headers per spec §1.2:
// x-api-key always; Authorization Bearer as well for oauth mode (the ZCode
// gateway path). The wire never carries a sealed enc:v1: blob.
func chatAuthHeaders(h http.Header, cred Credential) {
	if cred.IsOAuth() {
		token := strings.TrimSpace(cred.ZCodeJWT)
		if token == "" {
			token = strings.TrimSpace(cred.AccessToken)
		}
		if token != "" {
			h.Set("x-api-key", token)
			h.Set("Authorization", "Bearer "+token)
		}
		return
	}
	if key := strings.TrimSpace(cred.APIKey); key != "" {
		h.Set("x-api-key", key)
	}
}

// resolveCaps returns the catalog capabilities for the requested model,
// falling back to the static fallback list. The console default never locks
// a higher client value, matching the cross-provider reasoning contract.
func (c *Client) resolveCaps(ctx context.Context, accountID, model string) providers.ModelCapabilities {
	models, err := c.Models(ctx, accountID)
	if err != nil || len(models) == 0 {
		models = fallbackCatalog
	}
	canonical := accounts.CanonicalModelID(model)
	for _, m := range models {
		if m.PublicModel == model || m.NativeModel == model || accounts.CanonicalModelID(m.PublicModel) == canonical {
			return m.Capabilities
		}
	}
	return providers.ModelCapabilities{}
}

func (c *Client) chatRequest(ctx context.Context, accountID string, cred Credential, req translate.ChatRequest) (*http.Request, providers.ResolvedChat, error) {
	caps := c.resolveCaps(ctx, accountID, req.Model)
	stored := ""
	if reader, ok := c.store.(modelSettingReader); ok && accountID != "" {
		if s, err := reader.GetProviderModelSetting(ctx, "zcode", accounts.CanonicalModelID(req.Model)); err == nil {
			stored = s.ReasoningEffort
		}
	}
	level := requestedReasoningLevel(req)
	if level == "" {
		level = stored
	}
	level = providers.ResolveReasoningLevel(level, caps)
	body := anthropicBody(req, level, caps.MaxOutput)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(cred), bytes.NewReader(payload))
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	for k, v := range defaultHeaders() {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	chatAuthHeaders(httpReq.Header, cred)
	return httpReq, providers.ResolvedChat{ReasoningLevel: level}, nil
}

// modelSettingReader is the optional console-side reasoning store. It is
// matched by type assertion so the adapter does not need to know about the
// control plane's concrete store type.
type modelSettingReader interface {
	GetProviderModelSetting(ctx context.Context, provider, modelID string) (accounts.ProviderModelSetting, error)
}

// requestedReasoningLevel extracts a client-supplied reasoning hint from the
// OpenAI body (reasoning_effort string or {effort} object, plus the boolean
// thinking toggles). Returns "" when the client did not ask for one.
func requestedReasoningLevel(req translate.ChatRequest) string {
	if len(req.ReasoningEffort) > 0 {
		var value any
		if json.Unmarshal(req.ReasoningEffort, &value) == nil {
			switch typed := value.(type) {
			case string:
				if level := providers.NormalizeReasoningLevel(typed); level != "" {
					return level
				}
			case map[string]any:
				for _, key := range []string{"effort", "level", "type"} {
					if text, ok := typed[key].(string); ok {
						if level := providers.NormalizeReasoningLevel(text); level != "" {
							return level
						}
					}
				}
			}
		}
	}
	if req.EnableThinking != nil {
		if *req.EnableThinking {
			return "medium"
		}
		return "none"
	}
	if req.EnableReasoning != nil {
		if *req.EnableReasoning {
			return "medium"
		}
		return "none"
	}
	if req.IsReasoning != nil {
		if *req.IsReasoning {
			return "medium"
		}
		return "none"
	}
	return ""
}

// resolvedCredential loads the stored credential. Chat never rotates tokens:
// zcode OAuth refresh tokens are rotated by the official desktop client, so
// the chat path surfaces the stored payload as-is and lets a 401 classify
// into auth-failed, which the console surfaces as a re-login prompt.
func (c *Client) resolvedCredential(ctx context.Context, accountID string) (Credential, error) {
	_, payload, err := c.store.LoadCredentialPayload(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	return DecodeCredential(payload)
}

// ChatNonStream collects the upstream SSE stream into a single
// chat.completion-shaped outcome. The upstream is always asked to stream so
// the wire shape is uniform with ChatStream.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	httpReq, resolved, err := c.chatRequest(ctx, accountID, cred, req)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if readErr != nil {
		return providers.ChatOutcome{}, fmt.Errorf("read zcode stream: %w", readErr)
	}
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		return providers.ChatOutcome{}, classifiedHTTPError(resp.StatusCode, body)
	}
	aggregate, err := Aggregate(bytes.NewReader(body))
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	outcome, err := outcomeFromAggregate(aggregate)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	outcome.ReasoningLevel = resolved.ReasoningLevel
	return outcome, nil
}

// ChatStream issues the request and returns the rewritten OpenAI SSE body.
// Non-2xx responses are classified and returned as errors before the caller
// has committed to a 200, so cooldown and failover can apply.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	httpReq, resolved, err := c.chatRequest(ctx, accountID, cred, req)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, providers.ResolvedChat{}, classifiedHTTPError(resp.StatusCode, body)
	}
	return rewriteChatStream(resp), resolved, nil
}

func outcomeFromAggregate(aggregate map[string]any) (providers.ChatOutcome, error) {
	raw, err := json.Marshal(aggregate)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	var parsed struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int  `json:"prompt_tokens"`
			CompletionTokens int  `json:"completion_tokens"`
			CacheReadTokens  *int `json:"cache_read_tokens"`
			CacheWriteTokens *int `json:"cache_write_tokens"`
		} `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return providers.ChatOutcome{}, err
	}
	out := providers.ChatOutcome{UsageSource: "upstream", FinishReason: "stop"}
	if len(parsed.Choices) > 0 {
		out.Content = parsed.Choices[0].Message.Content
		out.Reasoning = parsed.Choices[0].Message.ReasoningContent
		out.ToolCalls = parsed.Choices[0].Message.ToolCalls
		if parsed.Choices[0].FinishReason != "" {
			out.FinishReason = parsed.Choices[0].FinishReason
		}
	}
	out.Model = parsed.Model
	out.PromptTokens = parsed.Usage.PromptTokens
	out.CompletionTokens = parsed.Usage.CompletionTokens
	out.CacheReadTokens = parsed.Usage.CacheReadTokens
	out.CacheWriteTokens = parsed.Usage.CacheWriteTokens
	return out, nil
}

// classifiedHTTPError maps an upstream non-2xx to a providers.Error using
// the shared taxonomy. The body is passed through verbatim in Message so
// operators can see the upstream's actual reason (per spec §1.2).
func classifiedHTTPError(status int, body []byte) error {
	classified := Classify(status, string(body))
	if classified.Kind == "" {
		// Unknown body shape: still surface a usable error so callers do not
		// fall through to "success with empty body".
		classified = providers.ClassifiedError{
			Kind:    accounts.KindUnavailable,
			Status:  status,
			Message: strings.TrimSpace(string(body)),
		}
	}
	return &providers.Error{
		Kind:    classified.Kind,
		Status:  classified.Status,
		Message: classified.Message,
	}
}

// Classify maps ZCode upstream failures to the shared taxonomy. Anthropic
// errors carry a {type:"error",error:{type,message}} envelope; the ZCode
// gateway uses {"code":N,"msg":"..."} for plan errors.
func Classify(status int, body string) providers.ClassifiedError {
	text := strings.ToLower(body)
	message := strings.TrimSpace(body)
	// Anthropic error envelope.
	var anthropicErr struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &anthropicErr) == nil && anthropicErr.Error.Type != "" {
		message = strings.TrimSpace(anthropicErr.Error.Message)
		switch anthropicErr.Error.Type {
		case "authentication_error", "permission_error":
			return providers.ClassifiedError{Kind: accounts.KindAuth, Status: 401, Message: message}
		case "rate_limit_error":
			return providers.ClassifiedError{Kind: accounts.KindRateLimit, Status: 429, Message: message}
		case "invalid_request_error", "not_found_error":
			return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: firstNonEmptyStatus(status, 400), Message: message}
		case "overloaded_error", "api_error":
			return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: firstNonEmptyStatus(status, 502), Message: message}
		}
	}
	switch {
	case status == 401 || status == 403:
		return providers.ClassifiedError{Kind: accounts.KindAuth, Status: status, Message: message}
	case status == 402 || strings.Contains(text, "insufficient") && (strings.Contains(text, "credit") || strings.Contains(text, "balance")) ||
		strings.Contains(text, "quota exceeded") || strings.Contains(text, "余额不足"):
		return providers.ClassifiedError{Kind: accounts.KindQuota, Status: 402, Message: message}
	case status == 429 || strings.Contains(text, "rate_limit") || strings.Contains(text, "rate limit"):
		return providers.ClassifiedError{Kind: accounts.KindRateLimit, Status: 429, Message: message}
	case status == 404:
		return providers.ClassifiedError{Kind: accounts.KindModelNotAvailable, Status: 404, Message: message}
	case status == 400 || accounts.IsPromptLimitText(body) || accounts.IsInvalidRequestText(body):
		return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: firstNonEmptyStatus(status, 400), Message: message}
	case status >= 500:
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: status, Message: message}
	}
	return providers.ClassifiedError{}
}

func firstNonEmptyStatus(status, fallback int) int {
	if status >= 400 {
		return status
	}
	return fallback
}

// classifier implements providers.ErrorClassifier for the adapter.
type classifier struct{}

func (classifier) Classify(status int, body string) providers.ClassifiedError {
	return Classify(status, body)
}
