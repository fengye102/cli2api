package autoclaw

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Credential is the canonical storage payload for provider=autoclaw /
// autoclaw-intl.
//
// AccessToken is a JWT sent as X-Authorization: Bearer *** on every LLM call.
// RefreshToken mints a new access token; the upstream rotates it on each
// refresh, so the stored copy must be updated after every successful refresh
// or the next refresh fails.
//
// Region is stored because the same desktop login file backs both builds: the
// file carries no region marker, so a credential that outlives an adapter
// restart has to remember which host it belongs to.
type Credential struct {
	Format       string `json:"format"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt is the JWT exp claim, RFC3339. Empty means unknown, in which
	// case the credential is treated as usable until the upstream rejects it.
	ExpiresAt string `json:"expires_at,omitempty"`
	// DeviceID is optional; the upstream accepts requests without it.
	DeviceID string `json:"device_id,omitempty"`
	// UserID is a stable identity hint from the JWT, used for console display.
	UserID string `json:"user_id,omitempty"`
	// Region records which build this credential belongs to.
	Region string `json:"region,omitempty"`
}

// DecodeCredential accepts the canonical form plus the aliases a pasted
// desktop auth.json or a console "paste a token" produces.
//
// The desktop file uses camelCase (refreshToken / deviceId), and the console's
// paste-a-token path hands a raw string over as {"api_key": ...}, so both must
// decode rather than error.
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
		ExpiresAt         string `json:"expires_at"`
		ExpiresAtCamel    string `json:"expiresAt"`
		DeviceID          string `json:"device_id"`
		DeviceIDCamel     string `json:"deviceId"`
		UserID            string `json:"user_id"`
		UserIDCamel       string `json:"userId"`
		Region            string `json:"region"`
	}
	if err := json.Unmarshal(payload, &alias); err != nil {
		// A bare JSON string, or a raw token pasted with no JSON around it, is
		// an access token. The console's "paste a token" path produces exactly
		// that shape, so it must not be an error.
		if bare, ok := bareToken(payload); ok {
			return Credential{Format: CredentialFormat, AccessToken: bare}, nil
		}
		return Credential{}, fmt.Errorf("autoclaw credential: %w", err)
	}
	cred := Credential{
		Format:       alias.Format,
		AccessToken:  firstNonEmpty(alias.AccessToken, alias.AccessTokenCamel, alias.Token, alias.APIKey, alias.APIKeyCamel, alias.Key),
		RefreshToken: firstNonEmpty(alias.RefreshToken, alias.RefreshTokenCamel),
		ExpiresAt:    firstNonEmpty(alias.ExpiresAt, alias.ExpiresAtCamel),
		DeviceID:     firstNonEmpty(alias.DeviceID, alias.DeviceIDCamel),
		UserID:       firstNonEmpty(alias.UserID, alias.UserIDCamel),
		Region:       alias.Region,
	}
	if cred.Format == "" {
		cred.Format = CredentialFormat
	}
	cred.AccessToken = trimBearer(cred.AccessToken)
	cred.RefreshToken = trimBearer(cred.RefreshToken)
	cred.DeviceID = strings.TrimSpace(cred.DeviceID)
	cred.UserID = strings.TrimSpace(cred.UserID)
	return cred, nil
}

// EncodeCredential serializes the canonical payload.
func EncodeCredential(cred Credential) ([]byte, error) {
	cred.AccessToken = trimBearer(cred.AccessToken)
	cred.RefreshToken = trimBearer(cred.RefreshToken)
	if cred.Format == "" {
		cred.Format = CredentialFormat
	}
	return json.Marshal(cred)
}

// ValidateCredential checks that the payload is a usable credential.
func ValidateCredential(payload []byte) error {
	cred, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return fmt.Errorf("autoclaw credential needs an access token or a refresh token")
	}
	return nil
}

// Ready reports whether the credential can serve a request now.
func (cred Credential) Ready() bool {
	return strings.TrimSpace(cred.AccessToken) != "" || strings.TrimSpace(cred.RefreshToken) != ""
}

// CanRefresh reports whether a refresh is possible at all. Credentials pasted
// as a bare access token have no refresh token, and pretending otherwise would
// make maintenance retry them forever.
func (cred Credential) CanRefresh() bool {
	return strings.TrimSpace(cred.RefreshToken) != ""
}

// Expiry returns the parsed expiry, or the zero time when unknown.
func (cred Credential) Expiry() time.Time {
	raw := strings.TrimSpace(cred.ExpiresAt)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// NeedsRefresh reports whether the token is inside the expiry window. An
// unknown expiry returns false: the upstream is the authority then, and a 401
// triggers a forced refresh.
func (cred Credential) NeedsRefresh(now time.Time) bool {
	expiry := cred.Expiry()
	if expiry.IsZero() {
		return false
	}
	return expiry.Sub(now) <= refreshExpiringWindow*time.Second
}

// Expired reports whether the token is already past its expiry.
func (cred Credential) Expired(now time.Time) bool {
	expiry := cred.Expiry()
	if expiry.IsZero() {
		return false
	}
	return now.After(expiry)
}

func trimBearer(value string) string {
	value = strings.TrimSpace(value)
	return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
}

// bareToken reads a payload that is not a JSON object: either a quoted JSON
// string or the raw token itself.
func bareToken(payload []byte) (string, bool) {
	var quoted string
	if err := json.Unmarshal(payload, &quoted); err == nil {
		quoted = trimBearer(quoted)
		return quoted, quoted != ""
	}
	raw := strings.TrimSpace(string(payload))
	if raw == "" || strings.ContainsAny(raw, "{[") {
		return "", false
	}
	raw = strings.TrimSpace(strings.Trim(raw, `"`))
	raw = trimBearer(raw)
	return raw, raw != ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
