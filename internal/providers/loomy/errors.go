package loomy

import (
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Classify maps a model-gateway status/body onto the internal taxonomy.
//
// The gateway answers OpenAI-shaped errors plus a few 讯飞 business codes, so
// the reading is:
//
//	401 / 403                 → auth        (session dead; chat re-logs in once)
//	429                       → rate_limit  (cool the account, honour retry-after)
//	402, or quota wording      → quota       (out of credits)
//	400                       → invalid_request (content screening, bad payload)
//	404                       → model_not_available
//	5xx / anything else        → unavailable (upstream hiccup, not the account)
func Classify(status int, body string) providers.ClassifiedError {
	message := trimBody(body)
	lower := strings.ToLower(message)

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return providers.ClassifiedError{Kind: accounts.KindAuth, Status: status, Message: message}
	case status == http.StatusTooManyRequests:
		return providers.ClassifiedError{Kind: accounts.KindRateLimit, Status: status, Message: message}
	case status == http.StatusPaymentRequired:
		return providers.ClassifiedError{Kind: accounts.KindQuota, Status: status, Message: message}
	case status == http.StatusNotFound:
		return providers.ClassifiedError{Kind: accounts.KindModelNotAvailable, Status: status, Message: message}
	}

	// The gateway reports "not enough credits" as a 200-with-error or a 400 in
	// some builds, so the wording decides before the status does.
	for _, marker := range []string{
		"insufficient", "no enough", "not enough", "quota", "credit",
		"积分不足", "余额不足", "额度不足", "积分", "额度",
	} {
		if strings.Contains(lower, marker) {
			return providers.ClassifiedError{Kind: accounts.KindQuota, Status: status, Message: message}
		}
	}

	if status == http.StatusBadRequest {
		return providers.ClassifiedError{Kind: accounts.KindInvalidRequest, Status: status, Message: message}
	}
	return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: status, Message: message}
}

// newProviderError builds the *providers.Error the executor classifies.
func newProviderError(status int, body string) *providers.Error {
	cls := Classify(status, body)
	return &providers.Error{Kind: cls.Kind, Status: status, Message: cls.Message}
}

func trimBody(body string) string {
	body = strings.TrimSpace(body)
	if len(body) > 512 {
		return body[:512]
	}
	return body
}
