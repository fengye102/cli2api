package runable

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Credential is the canonical storage payload for provider=runable.
//
// Runable has no API keys for third parties: the web client authenticates with
// its own cookies, so an account is a cookie header lifted from a signed-in
// browser. There is nothing to refresh — a dead session is a dead session — which
// is why the adapter parks the account on 401 instead of trying to renew.
//
//	{"format":"runable-cookie-v1","cookie":"__Secure-better-auth.session_token=…; …"}
//
// DeviceID is optional and only used when an export carries one; the device
// header itself is a constant (see DeviceHeader).
type Credential struct {
	Format string `json:"format,omitempty"`
	// Cookie is the full Cookie header value, without the "Cookie:" prefix.
	Cookie string `json:"cookie"`
	// Name is a display-only label for the account list.
	Name string `json:"name,omitempty"`
	// UserID/Email are display-only values an export may carry.
	UserID string `json:"user_id,omitempty"`
	Email  string `json:"email,omitempty"`
}

var cookiePrefix = regexp.MustCompile(`(?i)^\s*cookie\s*:\s*`)

// DecodeCredential accepts the canonical JSON, the aliases an export may use
// (cookie/raw/cookies/session_token), and a bare cookie string — a paste of the
// Cookie header is the most likely input of all.
func DecodeCredential(payload []byte) (Credential, error) {
	raw := strings.TrimSpace(string(payload))
	if raw == "" {
		return Credential{}, fmt.Errorf("runable credential 为空")
	}
	if !strings.HasPrefix(raw, "{") {
		var bare string
		if err := json.Unmarshal(payload, &bare); err == nil {
			raw = bare
		}
		cookie := NormalizeCookie(raw)
		if cookie == "" {
			return Credential{}, fmt.Errorf("runable credential 里没有 cookie")
		}
		return Credential{Format: CredentialFormat, Cookie: cookie}, nil
	}
	var alias struct {
		Format       string `json:"format"`
		Cookie       string `json:"cookie"`
		Cookies      string `json:"cookies"`
		Raw          string `json:"raw"`
		SessionToken string `json:"session_token"`
		Token        string `json:"token"`
		// APIKey is what the console's paste box wraps a token in (login/pat
		// sends {"api_key": …}), so a pasted Cookie header arrives here.
		APIKey      string `json:"api_key"`
		Name        string `json:"name"`
		UserID      string `json:"user_id"`
		UserIDCamel string `json:"userId"`
		Email       string `json:"email"`
	}
	if err := json.Unmarshal(payload, &alias); err != nil {
		return Credential{}, fmt.Errorf("runable credential: %w", err)
	}
	cookie := NormalizeCookie(firstNonEmpty(alias.Cookie, alias.Cookies, alias.Raw, alias.SessionToken, alias.Token, alias.APIKey))
	if cookie == "" {
		return Credential{}, fmt.Errorf("runable credential 里没有 cookie 字段")
	}
	cred := Credential{
		Format: firstNonEmpty(alias.Format),
		Cookie: cookie,
		Name:   strings.TrimSpace(alias.Name),
		UserID: firstNonEmpty(alias.UserID, alias.UserIDCamel),
		Email:  strings.TrimSpace(alias.Email),
	}
	if cred.Format == "" {
		cred.Format = CredentialFormat
	}
	return cred, nil
}

// NormalizeCookie strips a "Cookie:" prefix, folds newlines into a single header
// and rejects a paste that is obviously not a cookie.
func NormalizeCookie(raw string) string {
	value := cookiePrefix.ReplaceAllString(raw, "")
	value = strings.ReplaceAll(value, "\r\n", "; ")
	value = strings.ReplaceAll(value, "\r", "; ")
	value = strings.ReplaceAll(value, "\n", "; ")
	value = strings.TrimSpace(value)
	for strings.Contains(value, ";;") {
		value = strings.ReplaceAll(value, ";;", ";")
	}
	value = strings.TrimSuffix(value, ";")
	// A cookie header is name=value pairs; anything without an '=' is a paste
	// of the wrong thing (a bearer token, a URL), and sending it would only
	// produce a confusing 401 later.
	if !strings.Contains(value, "=") {
		return ""
	}
	return strings.TrimSpace(value)
}

// EncodeCredential serialises the canonical payload.
func EncodeCredential(cred Credential) ([]byte, error) {
	if cred.Format == "" {
		cred.Format = CredentialFormat
	}
	return json.Marshal(cred)
}

// ValidateCredential checks that the payload can serve a request.
func ValidateCredential(payload []byte) error {
	cred, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cred.Cookie) == "" {
		return fmt.Errorf("runable credential 需要网页 cookie")
	}
	return nil
}

// Ready reports whether the credential can be used immediately.
func (c Credential) Ready() bool { return strings.TrimSpace(c.Cookie) != "" }

// Label is the account label the console shows.
func (c Credential) Label() string {
	if label := strings.TrimSpace(c.Name); label != "" {
		return label
	}
	return firstNonEmpty(c.Email, c.UserID, Label)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
