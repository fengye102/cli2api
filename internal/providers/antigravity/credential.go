package antigravity

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Credential is the canonical storage payload for provider=antigravity.
//
// The provider has two auth modes and this one type carries both:
//
//	key   — a Gemini API key created in Google AI Studio. Chat, the model list
//	        and probing go through the OpenAI-compatibility layer.
//	oauth — a Google OAuth refresh token (the long-lived main credential) plus
//	        an optional access-token cache. Chat, the model list and project
//	        discovery go through Cloud Code Assist (v1internal), the protocol the
//	        Antigravity IDE itself speaks.
//
// Exactly one of APIKey / RefreshToken is required. AuthMode is derived when the
// stored value does not spell it out, so a credential written before the field
// existed still resolves correctly.
type Credential struct {
	Format   string `json:"format"`
	AuthMode string `json:"auth_mode,omitempty"`

	// Key mode.
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`

	// OAuth mode.
	RefreshToken string `json:"refresh_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	Email        string `json:"email,omitempty"`
}

// Auth modes.
const (
	// AuthModeKey is the pasted-Gemini-API-key mode.
	AuthModeKey = "key"
	// AuthModeOAuth is the browser-login (Google OAuth) mode.
	AuthModeOAuth = "oauth"
)

// FormatAPIKey trims a pasted key and an accidental Bearer prefix.
func FormatAPIKey(raw string) string {
	key := strings.TrimSpace(raw)
	key = strings.TrimPrefix(key, "Bearer ")
	key = strings.TrimPrefix(key, "bearer ")
	return strings.TrimSpace(key)
}

// aliasCredential is the permissive parse shape: every accepted spelling of
// every field, so one unmarshal covers both modes and all import dialects.
type aliasCredential struct {
	Format      string `json:"format"`
	AuthMode    string `json:"auth_mode"`
	APIKey      string `json:"api_key"`
	APIKeyCamel string `json:"apiKey"`
	Key         string `json:"key"`
	Token       string `json:"token"`
	BaseURL     string `json:"base_url"`
	BaseURLCam  string `json:"baseUrl"`

	RefreshToken string `json:"refresh_token"`
	RefreshCaml  string `json:"refreshToken"`
	AccessToken  string `json:"access_token"`
	AccessCaml   string `json:"accessToken"`
	ExpiresAt    int64  `json:"expires_at"`
	ExpiresAtCam int64  `json:"expiresAt"`
	ProjectID    string `json:"project_id"`
	ProjectIDCam string `json:"projectId"`
	Email        string `json:"email"`
}

// DecodeCredential accepts the canonical form plus a set of aliases so a pasted
// {"apiKey":…}, {"token":…} or {"refreshToken":…} still imports.
func DecodeCredential(payload []byte) (Credential, error) {
	var alias aliasCredential
	if err := json.Unmarshal(payload, &alias); err != nil {
		return Credential{}, fmt.Errorf("antigravity credential is not valid json")
	}
	credential := Credential{
		Format:       strings.TrimSpace(alias.Format),
		AuthMode:     strings.TrimSpace(alias.AuthMode),
		APIKey:       firstNonEmpty(alias.APIKey, alias.APIKeyCamel, alias.Key, alias.Token),
		BaseURL:      firstNonEmpty(alias.BaseURL, alias.BaseURLCam),
		RefreshToken: firstNonEmpty(alias.RefreshToken, alias.RefreshCaml),
		AccessToken:  firstNonEmpty(alias.AccessToken, alias.AccessCaml),
		ProjectID:    firstNonEmpty(alias.ProjectID, alias.ProjectIDCam),
		Email:        strings.TrimSpace(alias.Email),
	}
	if alias.ExpiresAt > 0 {
		credential.ExpiresAt = alias.ExpiresAt
	} else if alias.ExpiresAtCam > 0 {
		credential.ExpiresAt = alias.ExpiresAtCam
	}
	credential.APIKey = FormatAPIKey(credential.APIKey)
	credential.RefreshToken = strings.TrimSpace(credential.RefreshToken)
	credential.AccessToken = strings.TrimSpace(credential.AccessToken)
	credential.BaseURL = strings.TrimSpace(credential.BaseURL)
	credential.ProjectID = strings.TrimSpace(credential.ProjectID)
	credential.AuthMode = credential.Mode()
	if !credential.Ready() {
		return Credential{}, fmt.Errorf("antigravity credential requires an api key or a refresh token")
	}
	if credential.Format == "" {
		credential.Format = credential.defaultFormat()
	}
	return credential, nil
}

// Mode reports the auth mode: the explicit field when set, otherwise derived
// from which secret is present.
func (c Credential) Mode() string {
	switch strings.TrimSpace(c.AuthMode) {
	case AuthModeOAuth:
		return AuthModeOAuth
	case AuthModeKey:
		return AuthModeKey
	}
	if strings.TrimSpace(c.RefreshToken) != "" {
		return AuthModeOAuth
	}
	return AuthModeKey
}

// IsOAuth reports whether this credential drives the v1internal plane.
func (c Credential) IsOAuth() bool {
	return c.Mode() == AuthModeOAuth
}

// defaultFormat is the credential format for this credential's mode.
func (c Credential) defaultFormat() string {
	if c.IsOAuth() {
		return CredentialFormatOAuth
	}
	return CredentialFormat
}

// Encode normalizes and serializes the credential for storage.
func (c Credential) Encode() ([]byte, error) {
	c.AuthMode = c.Mode()
	c.Format = c.defaultFormat()
	c.APIKey = FormatAPIKey(c.APIKey)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.ProjectID = strings.TrimSpace(c.ProjectID)
	c.Email = strings.TrimSpace(c.Email)
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.IsOAuth() {
		// Key-mode fields are meaningless for an OAuth account; dropping them
		// keeps the stored payload honest about which mode it is.
		c.APIKey = ""
		c.BaseURL = ""
	} else {
		c.RefreshToken = ""
		c.AccessToken = ""
		c.ExpiresAt = 0
		c.ProjectID = ""
		c.Email = ""
	}
	return json.Marshal(c)
}

// Ready reports whether the credential can authenticate a request.
func (c Credential) Ready() bool {
	if c.IsOAuth() {
		return strings.TrimSpace(c.RefreshToken) != ""
	}
	return strings.TrimSpace(c.APIKey) != ""
}

// acceptChatBase reports whether an explicit base URL may be used. Tests in
// this package replace it to allow httptest's local http origin.
var acceptChatBase = func(parsed *url.URL) bool {
	return parsed != nil && strings.EqualFold(parsed.Scheme, "https") && parsed.Host != ""
}

// ChatBase returns the OpenAI-compatible origin for a key-mode credential.
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

// ValidateCredential is the CredentialCodec entry point. It accepts either mode.
func ValidateCredential(payload []byte) error {
	credential, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if !credential.Ready() {
		return fmt.Errorf("antigravity credential requires an api key or a refresh token")
	}
	if credential.IsOAuth() {
		return nil
	}
	if _, err := credential.ChatBase(); err != nil {
		return err
	}
	return nil
}

// ValidateOAuthCredential is the codec entry point for the browser-login format.
func ValidateOAuthCredential(payload []byte) error {
	credential, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if !credential.IsOAuth() {
		return fmt.Errorf("antigravity credential is not an oauth credential")
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
