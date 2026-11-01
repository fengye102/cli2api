package providers

import "regexp"

// secretPatterns are the shapes an upstream error body uses when it echoes the
// credential it just rejected: bearer tokens, Authorization/X-Auth-Token
// headers, Huawei's DPoP query-string pair, provider API keys as `"api_key":
// "..."`, and raw JWTs. Provider classifiers keep their own narrow copies for
// their own messages; this shared set exists for the one place every provider
// error passes through — the executor's log/client boundary — so a new provider
// cannot ship an unredacted error by forgetting to write its own redactor.
var secretPatterns = []*regexp.Regexp{
	// Header or query echo: "x-auth-token: abc", "Authorization=Bearer x".
	regexp.MustCompile(`(?i)(x-auth-token|x-security-token|x-api-key|x-goog-api-key|x-amz-security-token|authorization|proxy-authorization|cookie|set-cookie)\s*[:=]\s*\S+`),
	// key=value forms for anything whose name says it is a credential. The
	// separator may be quoted (`"api_key": "..."`, `api_key="..."`) and, inside
	// a JSON string, the quote may itself be escaped (`\"`).
	regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token|id[_-]?token|session[_-]?token|client[_-]?secret|secret[_-]?key|password|passwd)\b["']?\s*[:=]\s*\\?["']?[A-Za-z0-9._~+/=-]{6,}["']?`),
	// Huawei Cloud DPoP identity pair.
	regexp.MustCompile(`(?i)\bAccess=[^,\s&]+`),
	regexp.MustCompile(`(?i)\bSignature=[0-9a-f]+`),
	// "Bearer <token>" anywhere, including inside JSON string values.
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	// Raw JWT: header.payload.signature.
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{5,}`),
	// OpenAI-style and Anthropic-style key prefixes as a last resort.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}`),
}

// RedactSecrets masks credential material in an upstream error body before it
// reaches request logs, /api/logs, or the caller's HTTP response.
func RedactSecrets(text string) string {
	if text == "" {
		return text
	}
	safe := text
	for _, pattern := range secretPatterns {
		safe = pattern.ReplaceAllString(safe, "[redacted]")
	}
	return safe
}
