package minimax

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

var bearerPattern = regexp.MustCompile(`(?i)bearer\s+\S+`)

// redactSecrets keeps an upstream body out of the log minus the credential: the
// gateway echoes the Authorization header back on some refusals.
func redactSecrets(text string) string {
	if text == "" {
		return text
	}
	return bearerPattern.ReplaceAllString(text, "Bearer [redacted]")
}

// errText pulls the message out of the Anthropic error envelope the agent
// gateway uses: {"type":"error","error":{"type":"rate_limit_error","message":…}}.
func errText(body string) string {
	safe := redactSecrets(strings.TrimSpace(body))
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Message  string `json:"message"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if strings.HasPrefix(safe, "{") && json.Unmarshal([]byte(safe), &envelope) == nil {
		message := firstNonEmpty(envelope.Error.Message, envelope.Message, envelope.BaseResp.StatusMsg)
		kind := firstNonEmpty(envelope.Error.Type, envelope.Type)
		if message != "" {
			if kind != "" && kind != "error" {
				return message + " (" + kind + ")"
			}
			return message
		}
	}
	return safe
}

// Classify maps a MiniMax refusal onto the executor taxonomy.
//
// The taxonomy decides whether an account is cooled, so the mapping stays
// conservative: a 400 or a 404 is the caller's request, never a reason to pull a
// working account out of rotation.
func Classify(status int, body string) providers.ClassifiedError {
	text := strings.ToLower(errText(body))
	message := errText(body)
	switch {
	case status == 401 || status == 403 ||
		strings.Contains(text, "authentication_error") ||
		strings.Contains(text, "permission_error") ||
		strings.Contains(text, "invalid api key") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "invalid_grant"):
		return providers.ClassifiedError{Kind: accounts.KindAuth, Status: firstStatus(status, 401), Message: firstNonEmpty(message, "MiniMax 登录态已失效")}
	case status == 429 || strings.Contains(text, "rate_limit") || strings.Contains(text, "too many requests") ||
		strings.Contains(text, "slow_down"):
		return providers.ClassifiedError{Kind: accounts.KindRateLimit, Status: firstStatus(status, 429), Message: firstNonEmpty(message, "MiniMax 限流")}
	case status == 402 ||
		strings.Contains(text, "quota") ||
		strings.Contains(text, "insufficient") ||
		strings.Contains(text, "credit") ||
		strings.Contains(text, "balance") ||
		strings.Contains(text, "额度") ||
		strings.Contains(text, "积分") ||
		strings.Contains(text, "余额"):
		return providers.ClassifiedError{Kind: accounts.KindQuota, Status: firstStatus(status, 402), Message: firstNonEmpty(message, "MiniMax 额度不足")}
	case status == 404 || strings.Contains(text, "model_not_found") || strings.Contains(text, "not_found_error") ||
		strings.Contains(text, "does not exist"):
		return providers.ClassifiedError{Kind: accounts.KindModelNotAvailable, Status: 404, Message: firstNonEmpty(message, "MiniMax 没有这个模型")}
	case status == 400 || status == 422 ||
		strings.Contains(text, "invalid_request") ||
		accounts.IsInvalidRequestText(message) || accounts.IsPromptLimitText(message):
		return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: firstStatus(status, 400), Message: message}
	case status == 499 || strings.Contains(text, "canceled") || strings.Contains(text, "cancelled"):
		return providers.ClassifiedError{Kind: accounts.KindCanceled, Status: 499, Message: message}
	case status == 529 || strings.Contains(text, "overloaded"):
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: 529, Message: firstNonEmpty(message, "MiniMax 过载")}
	case status >= 500 || status == 0:
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: firstStatus(status, 503), Message: firstNonEmpty(message, "MiniMax 暂时不可用")}
	default:
		return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: firstStatus(status, 400), Message: message}
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
