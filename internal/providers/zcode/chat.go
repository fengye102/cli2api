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

// chatURL picks the upstream /v1/messages URL for the credential. An OAuth
// credential always goes to its region's plan gateway (PlanChatBase): the
// ZCode JWT is only accepted there, while the credential's stored base_url is
// the pay-as-you-go endpoint the desktop client exports and answers a JWT with
// 401. An API key goes to the stored base_url, then the region's ChatBase.
// The credential carries only the base; the path is fixed.
func chatURL(cred Credential) string {
	if region, ok := providers.ZCode.Region(cred.Provider); ok && cred.IsOAuth() {
		if plan := strings.TrimSpace(region.PlanChatBase); plan != "" {
			return strings.TrimRight(plan, "/") + "/v1/messages"
		}
	}
	if base := strings.TrimRight(strings.TrimSpace(cred.BaseURL), "/"); base != "" {
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
		// Plan channel: Bearer only. The desktop client never sends x-api-key
		// on the plan gateway (that is the coding-plan/API-key shape), and the
		// mixed shape is itself a risk signal.
		token := strings.TrimSpace(cred.ZCodeJWT)
		if token == "" {
			token = strings.TrimSpace(cred.AccessToken)
		}
		if token != "" {
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
	if cred.IsOAuth() {
		// The plan gateway reviews the system prompt and rejects a request
		// that lacks the official ZCode identity blocks with 3012.
		applyPlanIdentity(body, req.Model, deviceMid(accountID))
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(cred), bytes.NewReader(payload))
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	for k, v := range identityHeaders(accountID) {
		httpReq.Header.Set(k, v)
	}
	for k, v := range traceHeaders() {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	chatAuthHeaders(httpReq.Header, cred)
	if cred.IsOAuth() && c.captcha != nil && c.captcha.enabled() {
		param, region, err := c.captcha.param(ctx)
		if err != nil {
			return nil, providers.ResolvedChat{}, fmt.Errorf("zcode plan channel verification: %w", err)
		}
		httpReq.Header.Set(CaptchaHeader, param)
		httpReq.Header.Set(CaptchaRegionHeader, region)
	}
	return httpReq, providers.ResolvedChat{ReasoningLevel: level}, nil
}

// noteRiskResponse empties the verification pool when the upstream reports a
// risk block or a rejected token, so the next attempt solves a fresh one
// instead of replaying a burnt batch.
func (c *Client) noteRiskResponse(status int, body []byte) {
	if c.captcha == nil {
		return
	}
	if Classify(status, string(body)).Kind == accounts.KindRateLimit {
		c.captcha.invalidate()
	}
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
		c.noteRiskResponse(resp.StatusCode, body)
		return providers.ChatOutcome{}, classifiedHTTPError(resp.StatusCode, body)
	}
	if classified, ok := zcodeEnvelopeError(resp.StatusCode, body); ok {
		c.noteRiskResponse(classified.Status, body)
		return providers.ChatOutcome{}, &providers.Error{
			Kind:    classified.Kind,
			Status:  classified.Status,
			Message: classified.Message,
		}
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
		c.noteRiskResponse(resp.StatusCode, body)
		return nil, providers.ResolvedChat{}, classifiedHTTPError(resp.StatusCode, body)
	}
	// The plan gateway reports a spent entitlement or a risk block as a 2xx
	// {"code":N,"msg":...} JSON body rather than as a status code. It also
	// labels it application/json, while a real turn streams text/event-stream,
	// so the content type alone decides whether a body is worth reading: a
	// JSON-labelled 2xx is an error envelope, never a stream. Returning it as
	// a stream would hand the caller an empty turn that no account cooldown
	// ever sees — the agent then reports "returned no output".
	if !streamIsEventStream(resp.Header) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if classified, ok := zcodeEnvelopeError(resp.StatusCode, body); ok {
			c.noteRiskResponse(classified.Status, body)
			return nil, providers.ResolvedChat{}, &providers.Error{
				Kind:    classified.Kind,
				Status:  classified.Status,
				Message: classified.Message,
			}
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
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

// zcodeRiskText reports the plan gateway's risk-control block. The gateway
// answers an OAuth call it distrusts with HTTP 405 and
// {"code":3012,"msg":"request has been blocked due to unusual activity."} —
// the client is expected to solve an Aliyun 无痕验证 challenge and retry with
// X-Aliyun-Captcha-Verify-Param. It is a temporary upstream block, not a dead
// credential, so it must never be classified as auth.
func zcodeRiskText(text string) bool {
	return strings.Contains(text, "unusual activity") ||
		strings.Contains(text, `"code":3012`) ||
		strings.Contains(text, "captcha") ||
		strings.Contains(text, "verify token")
}

// zcodeQuotaText reports "this credential has no plan/balance left". The
// pay-as-you-go endpoint reports an unfunded account as a rate_limit_error
// envelope with code 1113, so the check runs before the envelope's own type
// mapping.
func zcodeQuotaText(text string) bool {
	for _, signal := range []string{
		"insufficient balance", "no resource package", "insufficient_quota",
		"quota exceeded", "余额不足", "no available quota", "resource package",
		"exceed quota limit", "exceed_quota", "quota limit",
	} {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

// streamIsEventStream reports whether the response advertises an SSE body. A
// real plan turn answers text/event-stream; the gateway's 2xx error envelope
// answers application/json, which is the only reliable signal separating a
// failed turn from an empty one (both carry status 200).
func streamIsEventStream(header http.Header) bool {
	if header == nil {
		return false
	}
	contentType := strings.ToLower(header.Get("Content-Type"))
	return strings.Contains(contentType, "text/event-stream")
}

// zcodeEnvelopeError reports the plan gateway's {\"code\":N,\"msg\":\"...\"} error
// shape when it arrives with a 2xx status. The gateway answers a spent
// entitlement (code 1005 \"exceed quota limit\"), a risk block (3012) and a
// rejected token (401) this way instead of using the HTTP status, so a caller
// that only inspects StatusCode treats the envelope as an ordinary stream and
// relays an empty body — the agent then reports \"returned no output\" with no
// account ever cooled down. Detecting it up front keeps the failure
// classifiable and lets failover pick a funded account.
//
// It returns the classified error and true only when the body is that envelope
// and carries a non-zero code; anything else (an Anthropic SSE frame, a normal
// aggregate, empty bytes) is left to the caller.
func zcodeEnvelopeError(status int, body []byte) (providers.ClassifiedError, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return providers.ClassifiedError{}, false
	}
	var envelope struct {
		Code json.Number `json:"code"`
		Msg  string      `json:"msg"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return providers.ClassifiedError{}, false
	}
	if envelope.Msg == "" {
		return providers.ClassifiedError{}, false
	}
	code, err := envelope.Code.Int64()
	if err != nil || code == 0 {
		return providers.ClassifiedError{}, false
	}
	classified := Classify(status, string(trimmed))
	if classified.Kind == "" {
		// An envelope the taxonomy does not recognise is still a failure;
		// never let it read as success.
		classified = providers.ClassifiedError{
			Kind:    accounts.KindUnavailable,
			Status:  firstNonEmptyStatus(status, http.StatusBadGateway),
			Message: strings.TrimSpace(string(trimmed)),
		}
	}
	if classified.Message == "" {
		classified.Message = strings.TrimSpace(string(trimmed))
	}
	return classified, true
}

// Classify maps ZCode upstream failures to the shared taxonomy. Anthropic
// errors carry a {type:"error",error:{type,message}} envelope; the ZCode
// gateway uses {"code":N,"msg":"..."} for plan errors.
func Classify(status int, body string) providers.ClassifiedError {
	text := strings.ToLower(body)
	message := strings.TrimSpace(body)
	// Risk control outranks every other reading: a blocked request says
	// nothing about the credential's validity.
	if zcodeRiskText(text) {
		return providers.ClassifiedError{
			Kind:    accounts.KindRateLimit,
			Status:  firstNonEmptyStatus(status, 429),
			Message: firstNonEmptyString(message, "request has been blocked due to unusual activity"),
		}
	}
	if zcodeQuotaText(text) {
		return providers.ClassifiedError{Kind: accounts.KindQuota, Status: firstNonEmptyStatus(status, 402), Message: message}
	}
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
