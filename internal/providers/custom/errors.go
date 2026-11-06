package custom

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

var bearerPattern = regexp.MustCompile(`(?i)bearer\s+\S+`)

// redactSecrets keeps a relay's error text out of the log minus the credential:
// some relays echo the Authorization header back in their error body.
func redactSecrets(text string) string {
	if text == "" {
		return text
	}
	return bearerPattern.ReplaceAllString(text, "Bearer [redacted]")
}

// errText pulls the human-readable message out of an OpenAI-style error
// envelope, falling back to the raw (redacted) body.
func errText(body string) string {
	safe := redactSecrets(strings.TrimSpace(body))
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if strings.HasPrefix(safe, "{") {
		if json.Unmarshal([]byte(safe), &envelope) == nil {
			message := firstNonEmpty(envelope.Error.Message, envelope.Message, envelope.Detail)
			if message != "" {
				if envelope.Error.Type != "" {
					return message + " (" + envelope.Error.Type + ")"
				}
				return message
			}
		}
	}
	return safe
}

// Classify maps a relay's HTTP error onto the executor taxonomy.
//
// The taxonomy decides whether an account gets cooled, so the mapping is
// conservative: only the statuses that genuinely describe this credential
// (auth, quota, rate limit) park it; a 400/404 is the caller's request or a
// model the relay does not serve, and must not remove a healthy account from
// rotation.
func Classify(status int, body string) providers.ClassifiedError {
	text := strings.ToLower(errText(body))
	message := errText(body)
	switch {
	case status == 401 || status == 403 ||
		strings.Contains(text, "invalid api key") ||
		strings.Contains(text, "invalid_api_key") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "authentication"):
		return providers.ClassifiedError{Kind: accounts.KindAuth, Status: firstStatus(status, 401), Message: firstNonEmpty(message, "上游拒绝了这个 key")}
	case status == 429 || strings.Contains(text, "rate limit") || strings.Contains(text, "too many requests") ||
		strings.Contains(text, "temporarily unavailable"):
		return providers.ClassifiedError{Kind: accounts.KindRateLimit, Status: firstStatus(status, 429), Message: firstNonEmpty(message, "上游限流")}
	case status == 402 ||
		strings.Contains(text, "insufficient") ||
		strings.Contains(text, "balance") ||
		strings.Contains(text, "quota") ||
		strings.Contains(text, "credit") ||
		strings.Contains(text, "余额") ||
		strings.Contains(text, "额度"):
		return providers.ClassifiedError{Kind: accounts.KindQuota, Status: firstStatus(status, 402), Message: firstNonEmpty(message, "上游额度不足")}
	case status == 404 || strings.Contains(text, "model not found") || strings.Contains(text, "does not exist"):
		return providers.ClassifiedError{Kind: accounts.KindModelNotAvailable, Status: 404, Message: firstNonEmpty(message, "上游没有这个模型")}
	case status == 400 || status == 422 || accounts.IsInvalidRequestText(message) || accounts.IsPromptLimitText(message):
		return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: firstStatus(status, 400), Message: message}
	case status == 499 || strings.Contains(text, "canceled") || strings.Contains(text, "cancelled"):
		return providers.ClassifiedError{Kind: accounts.KindCanceled, Status: 499, Message: message}
	case status >= 500 || status == 0:
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: firstStatus(status, 503), Message: firstNonEmpty(message, "上游暂时不可用")}
	default:
		return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: firstStatus(status, 400), Message: message}
	}
}

func firstStatus(status, fallback int) int {
	if status >= 100 {
		return status
	}
	return fallback
}
