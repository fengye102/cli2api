package runable

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

var bearerPattern = regexp.MustCompile(`(?i)bearer\s+\S+`)

// redactSecrets keeps a cookie or bearer value out of the log: the edge can echo
// the request headers back in an error body.
func redactSecrets(text string) string {
	if text == "" {
		return text
	}
	text = bearerPattern.ReplaceAllString(text, "Bearer [redacted]")
	return regexp.MustCompile(`(?i)(session_token=)[^;,\s]+`).ReplaceAllString(text, "${1}[redacted]")
}

// errText pulls the message out of an error body. The edge answers plain JSON
// ({"message":"You are not authenticated"}) as often as it answers a code.
func errText(body string) string {
	safe := redactSecrets(strings.TrimSpace(body))
	var envelope struct {
		Message string `json:"message"`
		Error   any    `json:"error"`
		Code    string `json:"code"`
		Detail  string `json:"detail"`
	}
	if strings.HasPrefix(safe, "{") && json.Unmarshal([]byte(safe), &envelope) == nil {
		switch value := envelope.Error.(type) {
		case string:
			if value != "" {
				return value
			}
		case map[string]any:
			if message, ok := value["message"].(string); ok && message != "" {
				return message
			}
		}
		if message := firstNonEmpty(envelope.Message, envelope.Detail); message != "" {
			return message
		}
	}
	return safe
}

// Classify maps a Runable refusal onto the executor taxonomy.
//
// The three statuses that describe this credential — 401 (the web session died),
// 402 (the free quota is spent) and 429 (rate limited) — park or cool the
// account. A 400 or a 404 is the caller's request and must not remove a healthy
// account from rotation.
func Classify(status int, body string) providers.ClassifiedError {
	text := strings.ToLower(errText(body))
	message := errText(body)
	switch {
	case status == 401 || status == 403 ||
		strings.Contains(text, "session_not_found") ||
		strings.Contains(text, "not authenticated") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "登录态") ||
		strings.Contains(text, "重新登录"):
		return providers.ClassifiedError{Kind: accounts.KindAuth, Status: firstStatus(status, 401),
			Message: firstNonEmpty(message, "Runable 网页登录态已失效，请重新粘贴 cookie")}
	case status == 402 ||
		strings.Contains(text, "out_of_credits") ||
		strings.Contains(text, "out of credits") ||
		strings.Contains(text, "insufficient") ||
		strings.Contains(text, "credit") ||
		strings.Contains(text, "quota") ||
		strings.Contains(text, "积分") ||
		strings.Contains(text, "额度"):
		return providers.ClassifiedError{Kind: accounts.KindQuota, Status: firstStatus(status, 402),
			Message: firstNonEmpty(message, "Runable 额度已用完")}
	case status == 429 ||
		strings.Contains(text, "rate limit") ||
		strings.Contains(text, "too many requests") ||
		strings.Contains(text, "too quickly"):
		return providers.ClassifiedError{Kind: accounts.KindRateLimit, Status: firstStatus(status, 429),
			Message: firstNonEmpty(message, "Runable 限流")}
	case status == 404 ||
		strings.Contains(text, "model not found") ||
		strings.Contains(text, "does not exist"):
		return providers.ClassifiedError{Kind: accounts.KindModelNotAvailable, Status: 404,
			Message: firstNonEmpty(message, "Runable 没有这个模型")}
	case status == 400 || status == 422 ||
		accounts.IsInvalidRequestText(message) || accounts.IsPromptLimitText(message):
		return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: firstStatus(status, 400),
			Message: message}
	case status == 499 || strings.Contains(text, "canceled") || strings.Contains(text, "cancelled"):
		return providers.ClassifiedError{Kind: accounts.KindCanceled, Status: 499, Message: message}
	case status >= 500 || status == 0:
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: firstStatus(status, 503),
			Message: firstNonEmpty(message, "Runable 暂时不可用")}
	default:
		return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: firstStatus(status, 400),
			Message: message}
	}
}

// newProviderError turns a classified refusal into the typed error the executor
// reads for cooldown and failover decisions.
func newProviderError(status int, body string) error {
	classified := Classify(status, body)
	return &providers.Error{
		Kind:    classified.Kind,
		Status:  classified.Status,
		Message: providers.RedactSecrets(classified.Message),
	}
}

func firstStatus(status, fallback int) int {
	if status >= 100 {
		return status
	}
	return fallback
}
