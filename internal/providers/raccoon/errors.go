package raccoon

import (
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Classify maps a Raccoon upstream status/body to the internal taxonomy:
//   - 401 → auth (refresh + retry happens in chat)
//   - 429 → rate limit (quota cooldown)
//   - 403 → auth or permission (treat as auth)
//   - else → fatal passthrough
func Classify(status int, body string) providers.ClassifiedError {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return providers.ClassifiedError{Kind: accounts.KindAuth, Status: status, Message: trimBody(body)}
	case http.StatusTooManyRequests:
		return providers.ClassifiedError{Kind: accounts.KindRateLimit, Status: status, Message: trimBody(body)}
	default:
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: status, Message: trimBody(body)}
	}
}

// newProviderError builds a *providers.Error from a status and body.
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
