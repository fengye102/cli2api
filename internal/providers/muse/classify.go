package muse

import (
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Classify maps a muse.ai HTTP status/body onto the internal taxonomy. muse.ai
// answers -1 style JSON errors ({"error": …}) rather than OpenAI envelopes, so
// the body is scanned for the phrases its web client shows.
func Classify(status int, body string) providers.ClassifiedError {
	lower := strings.ToLower(body)
	classified := providers.ClassifiedError{Status: status}
	switch {
	case status == 401 || status == 403 || strings.Contains(lower, "loggedout") || strings.Contains(lower, "not logged in"):
		classified.Kind = accounts.KindAuth
		classified.Message = "muse session expired; re-import cookies from a logged-in tab"
	case strings.Contains(lower, "quota") || strings.Contains(lower, "usage limit") || strings.Contains(lower, "out of credits") || strings.Contains(lower, "limit reached"):
		classified.Kind = accounts.KindQuota
		classified.Message = "muse usage limit reached"
	case status == 429 || strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests"):
		classified.Kind = accounts.KindRateLimit
		classified.Message = "muse rate limited this session"
	case status == 400 || status == 422:
		classified.Kind = accounts.KindInvalidRequest
		classified.Message = "muse rejected the request"
	case status == 404:
		classified.Kind = accounts.KindModelNotAvailable
		classified.Message = "muse has no such thread or model"
	case status >= 500:
		classified.Kind = accounts.KindUnavailable
		classified.Message = "muse upstream unavailable"
	default:
		classified.Kind = accounts.KindUnavailable
		classified.Message = "muse upstream error"
	}
	return classified
}

// Format satisfies providers.CredentialImporter. The console reaches the
// importer through adapter.Credential, so the codec itself has to expose the
// format and the normaliser; without it every import is rejected as an
// unsupported format even though the provider is registered.
func (credentialCodec) Format() string { return CredentialFormat }

func (credentialCodec) PrepareImport(payload []byte) (providers.CredentialImport, error) {
	return importer{}.PrepareImport(payload)
}
