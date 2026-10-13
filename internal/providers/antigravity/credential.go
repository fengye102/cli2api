package antigravity

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Credential is the canonical storage payload for provider=antigravity.
//
// Auth is one Gemini API key created in Google AI Studio. The key is used for
// chat, the model list, and readiness probing. BaseURL may override the pinned
// official origin (for tests or a self-hosted Gemini-compatible gateway); it
// must be an https URL without credentials, query, or fragment.
type Credential struct {
	Format  string `json:"format"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url,omitempty"`
}

// FormatAPIKey trims a pasted key and an accidental Bearer prefix.
func FormatAPIKey(raw string) string {
	key := strings.TrimSpace(raw)
	key = strings.TrimPrefix(key, "Bearer ")
	key = strings.TrimPrefix(key, "bearer ")
	return strings.TrimSpace(key)
}

// DecodeCredential accepts the canonical flat form or a small set of aliases
// so a pasted {"apiKey":…} or {"token":…} still imports.
func DecodeCredential(payload []byte) (Credential, error) {
	var flat Credential
	if err := json.Unmarshal(payload, &flat); err != nil {
		return Credential{}, fmt.Errorf("antigravity credential is not valid json")
	}
	if strings.TrimSpace(flat.APIKey) == "" {
		var alias struct {
			Format      string `json:"format"`
			APIKey      string `json:"api_key"`
			APIKeyCamel string `json:"apiKey"`
			Key         string `json:"key"`
			Token       string `json:"token"`
			BaseURL     string `json:"base_url"`
			BaseURLCam  string `json:"baseUrl"`
		}
		if err := json.Unmarshal(payload, &alias); err != nil {
			return Credential{}, fmt.Errorf("antigravity credential is not valid json")
		}
		flat.APIKey = firstNonEmpty(alias.APIKey, alias.APIKeyCamel, alias.Key, alias.Token)
		if flat.Format == "" {
			flat.Format = firstNonEmpty(alias.Format, CredentialFormat)
		}
		flat.BaseURL = firstNonEmpty(alias.BaseURL, alias.BaseURLCam)
	}
	flat.APIKey = FormatAPIKey(flat.APIKey)
	flat.BaseURL = strings.TrimSpace(flat.BaseURL)
	if strings.TrimSpace(flat.APIKey) == "" {
		return Credential{}, fmt.Errorf("antigravity credential requires an api key")
	}
	if flat.Format == "" {
		flat.Format = CredentialFormat
	}
	return flat, nil
}

// Encode normalizes and serializes the credential for storage.
func (c Credential) Encode() ([]byte, error) {
	c.Format = CredentialFormat
	c.APIKey = FormatAPIKey(c.APIKey)
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	return json.Marshal(c)
}

// Ready reports whether the credential can authenticate a request.
func (c Credential) Ready() bool {
	return strings.TrimSpace(c.APIKey) != ""
}

// acceptChatBase reports whether an explicit base URL may be used. Tests in
// this package replace it to allow httptest's local http origin.
var acceptChatBase = func(parsed *url.URL) bool {
	return parsed != nil && strings.EqualFold(parsed.Scheme, "https") && parsed.Host != ""
}

// ChatBase returns the OpenAI-compatible origin for this credential.
// An explicit https base URL wins; otherwise the pinned official host.
func (c Credential) ChatBase() (string, error) {
	if raw := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || !acceptChatBase(parsed) {
			return "", fmt.Errorf("antigravity base_url must be an https URL")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("antigravity base_url must not carry credentials, a query, or a fragment")
		}
		return raw, nil
	}
	return BaseURL, nil
}

// ValidateCredential is the CredentialCodec entry point.
func ValidateCredential(payload []byte) error {
	credential, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if !credential.Ready() {
		return fmt.Errorf("antigravity credential requires an api key")
	}
	if _, err := credential.ChatBase(); err != nil {
		return err
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
