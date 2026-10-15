package raccoon

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Credential is the canonical storage payload for provider=raccoon.
//
// Auth is an access token (JWT-shaped) plus the refresh token used to mint a
// new access token when the old one expires. The access token is sent as
// Authorization: Bearer <access_token> on every LLM call.
type Credential struct {
	Format       string `json:"format"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// UserID is a stable identity hint (from the JWT name/iss claims when
	// available); used only for console display.
	UserID string `json:"user_id,omitempty"`
}

// DecodeCredential accepts the canonical form or a small set of aliases so a
// pasted {"token":…} / {"refreshToken":…} still imports.
//
// api_key / key are accepted as spellings of the access token because the
// console's "paste a token" path hands the raw string over as {"api_key": …}
// (see control.LoginPAT). Raccoon has no personal access token of its own, but
// the web client's access token is exactly what that field carries.
func DecodeCredential(payload []byte) (Credential, error) {
	var alias struct {
		Format            string `json:"format"`
		AccessToken       string `json:"access_token"`
		AccessTokenCamel  string `json:"accessToken"`
		Token             string `json:"token"`
		APIKey            string `json:"api_key"`
		APIKeyCamel       string `json:"apiKey"`
		Key               string `json:"key"`
		RefreshToken      string `json:"refresh_token"`
		RefreshTokenCamel string `json:"refreshToken"`
		UserID            string `json:"user_id"`
	}
	if err := json.Unmarshal(payload, &alias); err != nil {
		// A bare JSON string — or a raw token pasted with no JSON around it —
		// is an access token. This is the shape the console's "paste a token"
		// path can produce, so it must not be an error.
		if bare, ok := bareToken(payload); ok {
			return Credential{Format: CredentialFormat, AccessToken: bare}, nil
		}
		return Credential{}, fmt.Errorf("raccoon credential: %w", err)
	}
	cred := Credential{
		Format:       alias.Format,
		AccessToken:  firstNonEmpty(alias.AccessToken, alias.AccessTokenCamel, alias.Token, alias.APIKey, alias.APIKeyCamel, alias.Key),
		RefreshToken: firstNonEmpty(alias.RefreshToken, alias.RefreshTokenCamel),
		UserID:       alias.UserID,
	}
	if cred.Format == "" {
		cred.Format = CredentialFormat
	}
	cred.AccessToken = strings.TrimSpace(strings.TrimPrefix(cred.AccessToken, "Bearer "))
	cred.RefreshToken = strings.TrimSpace(cred.RefreshToken)
	return cred, nil
}

// bareToken reads a payload that is not a JSON object: either a quoted JSON
// string or the raw token itself.
func bareToken(payload []byte) (string, bool) {
	var quoted string
	if err := json.Unmarshal(payload, &quoted); err == nil {
		quoted = strings.TrimSpace(strings.TrimPrefix(quoted, "Bearer "))
		return quoted, quoted != ""
	}
	raw := strings.TrimSpace(string(payload))
	if raw == "" || strings.ContainsAny(raw, "{[]") {
		return "", false
	}
	raw = strings.TrimSpace(strings.Trim(raw, "\""))
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	return raw, raw != ""
}

// EncodeCredential serializes the canonical payload.
func EncodeCredential(cred Credential) ([]byte, error) {
	cred.AccessToken = strings.TrimSpace(cred.AccessToken)
	cred.RefreshToken = strings.TrimSpace(cred.RefreshToken)
	if cred.Format == "" {
		cred.Format = "raccoon-credential-v1"
	}
	return json.Marshal(cred)
}

// ValidateCredential checks that the payload is a usable raccoon credential.
func ValidateCredential(payload []byte) error {
	cred, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return fmt.Errorf("raccoon credential needs an access token or a refresh token")
	}
	return nil
}

// Ready reports whether the credential can serve a chat request now.
func (cred Credential) Ready() bool {
	return strings.TrimSpace(cred.AccessToken) != "" || strings.TrimSpace(cred.RefreshToken) != ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
