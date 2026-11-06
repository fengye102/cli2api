package minimax

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Credential is the canonical storage payload for provider=minimax.
//
// It is an OAuth pair, not a cookie and not a static key: the access token dies
// after about twelve hours and the refresh token is single use. Both therefore
// have to be stored, together with the deadline, or the account cannot renew
// itself without a human.
//
//	{"format":"minimax-oauth-v1","access_token":"…","refresh_token":"…","expires_at":1794019934}
//
// Region selects the site the pair was issued by: a token minted on
// account.minimax.cn is not accepted by agent.minimax.io.
type Credential struct {
	Format string `json:"format,omitempty"`

	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt is the access token deadline in Unix seconds (0 = unknown).
	// A zero value is not treated as expired: an imported token with no
	// deadline is tried, and a 401 triggers a refresh.
	ExpiresAt int64 `json:"expires_at,omitempty"`

	// Region is the site id ("cn" or "global").
	Region string `json:"region,omitempty"`

	// TokenType is whatever the token endpoint reported; kept so a refresh can
	// reproduce the Authorization scheme exactly.
	TokenType string `json:"token_type,omitempty"`

	// Subject is a display-only hint (account e-mail or phone, when the export
	// carries one). It never travels in a request.
	Subject string `json:"subject,omitempty"`
}

// credentialAlias mirrors Credential plus every spelling an operator's export
// can realistically contain: the token endpoint's snake_case, the bridge's
// camelCase, and the tokens.json envelope minmaxcode2api writes.
type credentialAlias struct {
	Format string `json:"format"`

	AccessToken      string `json:"access_token"`
	AccessTokenCamel string `json:"accessToken"`
	AccessTokenCaps  string `json:"accessTokenValue"`
	Token            string `json:"token"`
	// APIKey is what the console's paste box wraps a token in (login/pat sends
	// {"api_key": …}), so a pasted access token arrives under this name.
	APIKey string `json:"api_key"`

	RefreshToken      string `json:"refresh_token"`
	RefreshTokenCamel string `json:"refreshToken"`

	ExpiresAt      int64 `json:"expires_at"`
	ExpiresAtCamel int64 `json:"expiresAt"`
	// ExpiresIn is what the token endpoint returns (seconds).
	ExpiresIn int64 `json:"expires_in"`

	Region      string `json:"region"`
	Site        string `json:"site"`
	TokenType   string `json:"token_type"`
	TokenTypeC  string `json:"tokenType"`
	Subject     string `json:"subject"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	DisplayName string `json:"name"`

	// Envelope is the tokens.json shape minimaxcode2api keeps on disk:
	// {"cn":{"accessToken":…,"refreshToken":…,"expiresAt":<ms>},"global":{…}}.
	CN     *credentialAlias `json:"cn"`
	Global *credentialAlias `json:"global"`
}

// DecodeCredential accepts the canonical form, the alias spellings, the
// bridge's tokens.json envelope, and a bare access token string (what the
// console's paste box produces).
func DecodeCredential(payload []byte) (Credential, error) {
	raw := strings.TrimSpace(string(payload))
	if raw == "" {
		return Credential{}, fmt.Errorf("minimax credential 为空")
	}
	// A bare token: not JSON, or a JSON string. Both are a paste of one token.
	if !strings.HasPrefix(raw, "{") {
		var bare string
		if err := json.Unmarshal(payload, &bare); err == nil {
			raw = strings.TrimSpace(bare)
		}
		raw = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
		if raw == "" || strings.ContainsAny(raw, "{}[]") {
			return Credential{}, fmt.Errorf("minimax credential 无法解析")
		}
		return Credential{
			Format:      CredentialFormat,
			AccessToken: raw,
			Region:      siteCN.Region,
		}, nil
	}

	var alias credentialAlias
	if err := json.Unmarshal(payload, &alias); err != nil {
		return Credential{}, fmt.Errorf("minimax credential: %w", err)
	}
	// Unwrap the bridge envelope: pick the site entry that actually holds a
	// token, preferring the one named by the envelope's own region hints.
	region := firstNonEmpty(alias.Region, alias.Site)
	if alias.CN != nil && strings.TrimSpace(firstNonEmpty(alias.CN.AccessToken, alias.CN.AccessTokenCamel, alias.CN.Token)) != "" {
		inner := *alias.CN
		region = firstNonEmpty(region, siteCN.Region)
		alias = mergeAlias(inner, alias)
	} else if alias.Global != nil && strings.TrimSpace(firstNonEmpty(alias.Global.AccessToken, alias.Global.AccessTokenCamel, alias.Global.Token)) != "" {
		inner := *alias.Global
		region = firstNonEmpty(region, siteGlobal.Region)
		alias = mergeAlias(inner, alias)
	}

	cred := Credential{
		Format:       firstNonEmpty(alias.Format),
		AccessToken:  firstNonEmpty(alias.AccessToken, alias.AccessTokenCamel, alias.AccessTokenCaps, alias.Token, alias.APIKey),
		RefreshToken: firstNonEmpty(alias.RefreshToken, alias.RefreshTokenCamel),
		Region:       region,
		TokenType:    firstNonEmpty(alias.TokenType, alias.TokenTypeC),
		Subject:      firstNonEmpty(alias.Subject, alias.Email, alias.Phone, alias.DisplayName),
	}
	cred.ExpiresAt = normalizeDeadline(firstNonZero(alias.ExpiresAt, alias.ExpiresAtCamel), alias.ExpiresIn)
	cred.AccessToken = formatToken(cred.AccessToken)
	cred.RefreshToken = strings.TrimSpace(cred.RefreshToken)
	if cred.Format == "" {
		cred.Format = CredentialFormat
	}
	if cred.Region == "" {
		cred.Region = siteCN.Region
	}
	return cred, nil
}

// mergeAlias keeps the outer envelope's region-ish fields and takes the tokens
// from the inner site entry.
func mergeAlias(inner, outer credentialAlias) credentialAlias {
	if inner.Region == "" {
		inner.Region = outer.Region
	}
	if inner.Site == "" {
		inner.Site = outer.Site
	}
	return inner
}

// normalizeDeadline turns whatever the token endpoint or an export reported into
// Unix seconds. MiniMax has shipped seconds, milliseconds and a millisecond
// epoch in the same field, so all three are recognised:
//
//	1794019934      → already seconds
//	1794019934567   → millisecond epoch
//	43200           → seconds from now
func normalizeDeadline(expiresAt, expiresIn int64) int64 {
	value := expiresAt
	if value == 0 {
		value = expiresIn
	}
	switch {
	case value <= 0:
		return 0
	case value > 1e15: // microseconds
		return value / 1e6
	case value > 1e11: // milliseconds
		return value / 1e3
	case value > 1e9: // already a Unix second timestamp
		return value
	default: // a duration in seconds
		return time.Now().Add(time.Duration(value) * time.Second).Unix()
	}
}

// formatToken trims a pasted token and an accidental scheme prefix.
func formatToken(raw string) string {
	token := strings.TrimSpace(raw)
	for _, prefix := range []string{"Bearer ", "bearer ", "BEARER "} {
		token = strings.TrimPrefix(token, prefix)
	}
	return strings.TrimSpace(token)
}

// EncodeCredential serialises the canonical payload.
func EncodeCredential(cred Credential) ([]byte, error) {
	if cred.Format == "" {
		cred.Format = CredentialFormat
	}
	if cred.Region == "" {
		cred.Region = siteCN.Region
	}
	return json.Marshal(cred)
}

// ValidateCredential checks that the payload can serve a request now or after
// one refresh.
func ValidateCredential(payload []byte) error {
	cred, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cred.AccessToken) == "" && strings.TrimSpace(cred.RefreshToken) == "" {
		return fmt.Errorf("minimax credential 需要 access_token 或 refresh_token")
	}
	return nil
}

// Ready reports whether the credential holds something the gateway can be
// asked with immediately.
func (c Credential) Ready() bool {
	return strings.TrimSpace(c.AccessToken) != "" || strings.TrimSpace(c.RefreshToken) != ""
}

// CanRefresh reports whether the adapter can mint a new access token by itself.
func (c Credential) CanRefresh() bool {
	return strings.TrimSpace(c.RefreshToken) != ""
}

// NeedsRefresh reports whether the access token is missing or close enough to
// its deadline that spending a refresh now is cheaper than a 401 later.
func (c Credential) NeedsRefresh(now time.Time) bool {
	if strings.TrimSpace(c.AccessToken) == "" {
		return c.CanRefresh()
	}
	if c.ExpiresAt <= 0 {
		return false
	}
	return now.Add(refreshLead).Unix() >= c.ExpiresAt
}

// Site resolves the credential's region onto a deployment.
func (c Credential) Site() Site {
	return siteFor(c.Region)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
