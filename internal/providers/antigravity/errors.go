package antigravity

import (
	"regexp"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

var bearerPattern = regexp.MustCompile(`(?i)bearer\s+\S+`)

// locationBlockedText reports whether an upstream body is Google's
// region-restriction rejection. Cloud Code Assist refuses requests from
// unsupported countries with "User location is not supported for the API use."
// and FAILED_PRECONDITION. It is not a credential or quota problem: the request
// is well formed and the account is healthy, but the gateway's egress region is
// one Google will not serve. Retrying, refreshing tokens, or rotating accounts
// all change nothing — only a supported-region egress does.
func locationBlockedText(text string) bool {
	return strings.Contains(text, "user location is not supported") ||
		strings.Contains(text, "location is not supported")
}

// locationBlockedMessage is the operator-facing explanation. It names the cause
// and the fix, because the raw upstream text ("location is not supported") reads
// like a client bug rather than an infrastructure one.
const locationBlockedMessage = "Antigravity 拒绝当前出口地区（User location is not supported）：" +
	"Google 不对该出口 IP 所在地区提供服务。请给该账号配置受支持地区的代理" +
	"（账号级 proxy_url，http/socks5 均可），或把网关出口换到受支持地区后重试。" +
	"这不是账号或额度问题，换账号、刷新令牌都不会生效。"

func redactSecrets(text string) string {
	if text == "" {
		return text
	}
	return bearerPattern.ReplaceAllString(text, "Bearer [redacted]")
}

// Classify maps Antigravity/Gemini API errors onto the executor taxonomy.
// 401/403 cool the account as auth. 429 is a rate limit. 402 and an explicit
// balance or quota message are quota. Other 4xx are the request, not the
// account.
func Classify(status int, body string) providers.ClassifiedError {
	safe := redactSecrets(strings.TrimSpace(body))
	text := strings.ToLower(safe)
	// Region restriction is checked before the generic 400 arm: Google returns
	// it as FAILED_PRECONDITION/400, which would otherwise be classified as a
	// bad request and blame the caller for an infrastructure problem.
	if locationBlockedText(text) {
		return providers.ClassifiedError{
			Kind:    accounts.KindUnavailable,
			Status:  503,
			Message: locationBlockedMessage,
		}
	}
	switch {
	case status == 401 || status == 403 ||
		strings.Contains(text, "invalid api key") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "authentication") ||
		strings.Contains(text, "api key not valid") ||
		strings.Contains(text, "permission denied"):
		return providers.ClassifiedError{
			Kind:    accounts.KindAuth,
			Status:  firstStatus(status, 401),
			Message: firstNonEmpty(safe, "invalid antigravity api key"),
		}
	case status == 429 || strings.Contains(text, "rate limit") || strings.Contains(text, "too many requests"):
		return providers.ClassifiedError{
			Kind:    accounts.KindRateLimit,
			Status:  firstStatus(status, 429),
			Message: firstNonEmpty(safe, "antigravity rate limited"),
		}
	case status == 402 ||
		strings.Contains(text, "insufficient") ||
		strings.Contains(text, "balance") ||
		strings.Contains(text, "quota") ||
		strings.Contains(text, "billing"):
		return providers.ClassifiedError{
			Kind:    accounts.KindQuota,
			Status:  firstStatus(status, 402),
			Message: firstNonEmpty(safe, "antigravity quota exhausted"),
		}
	case status == 404:
		return providers.ClassifiedError{Kind: accounts.KindModelNotAvailable, Status: 404, Message: safe}
	case status == 400 || status == 422 || accounts.IsInvalidRequestText(safe) || accounts.IsPromptLimitText(safe):
		return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: firstStatus(status, 400), Message: safe}
	case status == 499 || strings.Contains(text, "canceled"):
		return providers.ClassifiedError{Kind: accounts.KindCanceled, Status: 499, Message: safe}
	case status >= 500 || status == 0:
		return providers.ClassifiedError{
			Kind:    accounts.KindUnavailable,
			Status:  firstStatus(status, 502),
			Message: firstNonEmpty(safe, "antigravity upstream unavailable"),
		}
	}
	if safe != "" && status >= 400 {
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: status, Message: safe}
	}
	return providers.ClassifiedError{}
}

func newProviderError(status int, body string) *providers.Error {
	classified := Classify(status, body)
	return &providers.Error{
		Kind:    classified.Kind,
		Status:  classified.Status,
		Message: classified.Message,
	}
}

func firstStatus(status, fallback int) int {
	if status > 0 {
		return status
	}
	return fallback
}
