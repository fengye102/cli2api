// Package codex implements the OpenAI Codex (ChatGPT subscription) in-process
// provider. Protocol constants live only in this package.
//
// Ported from github.com/router-for-me/CLIProxyAPI (MIT).
package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

const (
	CredentialFormat = "codex-oauth-v1"

	// OAuth endpoints and the public client_id the official codex CLI uses.
	AuthURL   = "https://auth.openai.com/oauth/authorize"
	TokenURL  = "https://auth.openai.com/oauth/token"
	ClientID  = "app_EMoamEEZ73f0CkXaXp7hrann"
	Scope     = "openid email profile offline_access"
	AuthScope = "openid profile email"

	// LoopbackAddr is the fixed redirect the OpenAI OAuth client registers.
	// Unlike Trae's random-port loopback this port is part of the registered
	// redirect_uri, so it cannot change.
	LoopbackAddr = "127.0.0.1:1455"
	pathCallback = "/auth/callback"
	redirectURI  = "http://localhost:1455/auth/callback"

	ChatBase      = "https://chatgpt.com/backend-api/codex"
	pathResponses = "/responses"
	// pathWhamUsage is the account usage probe. It returns the same rate_limits
	// object the websocket codex.rate_limits event carries, without spending a
	// chat turn. CLIProxyAPI reads those fields from response headers instead.
	pathWhamUsage = "/wham/usage"

	// Cloaking: upstream gates on the official codex CLI UA/originator.
	UserAgent  = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	Originator = "codex-tui"

	QuotaUnit = "rate_limit_window"
)

const (
	refreshLead     = 5 * time.Minute
	loginPendingTTL = 10 * time.Minute
)

// Credential is the canonical storage payload shape. Field names match the
// CodexTokenStorage JSON CLIProxyAPI writes so import/export round-trips.
type Credential struct {
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email,omitempty"`
	// ExpiresAt is a Unix second timestamp for the access token.
	ExpiresAt   int64  `json:"expires_at"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

func DecodeCredential(payload []byte) (Credential, error) {
	var flat Credential
	if err := json.Unmarshal(payload, &flat); err == nil &&
		(flat.AccessToken != "" || flat.RefreshToken != "") {
		flat.ExpiresAt = unixSeconds(flat.ExpiresAt)
		return flat, nil
	}
	// Accept CLIProxyAPI's nested token_data shape on import.
	var nested struct {
		TokenData struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
			Email        string `json:"email"`
			Expire       string `json:"expired"`
		} `json:"token_data"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(payload, &nested); err == nil &&
		(nested.TokenData.AccessToken != "" || nested.TokenData.RefreshToken != "") {
		return Credential{
			IDToken:      nested.TokenData.IDToken,
			AccessToken:  nested.TokenData.AccessToken,
			RefreshToken: nested.TokenData.RefreshToken,
			AccountID:    nested.TokenData.AccountID,
			Email:        nested.TokenData.Email,
			ExpiresAt:    parseExpire(nested.TokenData.Expire),
			LastRefresh:  nested.LastRefresh,
		}, nil
	}
	// Raw ~/.codex/auth.json (official CLI): OAuth tokens sit under "tokens".
	var authFile struct {
		AuthMode     string `json:"auth_mode"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
		Tokens       struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(payload, &authFile); err == nil &&
		(authFile.Tokens.AccessToken != "" || authFile.Tokens.RefreshToken != "") {
		credential := Credential{
			IDToken:      authFile.Tokens.IDToken,
			AccessToken:  authFile.Tokens.AccessToken,
			RefreshToken: authFile.Tokens.RefreshToken,
			AccountID:    authFile.Tokens.AccountID,
			LastRefresh:  authFile.LastRefresh,
		}
		fillFromJWTClaims(&credential)
		return credential, nil
	}
	// cockpit-tools backup export: {"email": …, "token": {access_token, refresh_token, id_token, …}}.
	var cockpit struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		Token struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"token"`
	}
	if err := json.Unmarshal(payload, &cockpit); err == nil &&
		(cockpit.Token.AccessToken != "" || cockpit.Token.RefreshToken != "") {
		credential := Credential{
			IDToken:      cockpit.Token.IDToken,
			AccessToken:  cockpit.Token.AccessToken,
			RefreshToken: cockpit.Token.RefreshToken,
			AccountID:    cockpit.Token.AccountID,
			Email:        cockpit.Email,
		}
		fillFromJWTClaims(&credential)
		return credential, nil
	}
	// Generic tolerant scan: any nested blob carrying codex token fields.
	if value, err := providers.UnwrapJSONValue(payload); err == nil {
		if auth := providers.DeepFindAuthObject(value); auth != nil {
			credential := Credential{
				AccessToken:  firstJSONString(auth, "access_token", "accessToken"),
				RefreshToken: firstJSONString(auth, "refresh_token", "refreshToken"),
				IDToken:      firstJSONString(auth, "id_token", "idToken"),
				AccountID:    firstJSONString(auth, "account_id", "accountId"),
			}
			if credential.AccessToken != "" || credential.RefreshToken != "" {
				fillFromJWTClaims(&credential)
				return credential, nil
			}
		}
	}
	return Credential{}, fmt.Errorf("codex credential requires access_token or refresh_token")
}

// fillFromJWTClaims derives AccountID/Email from id_token or access_token
// claims when the pasted payload does not carry them, mirroring what the
// official auth.json and cockpit-tools backups provide separately.
func fillFromJWTClaims(credential *Credential) {
	for _, token := range []string{credential.IDToken, credential.AccessToken} {
		applyJWTIdentity(credential, token)
		if credential.AccountID != "" && credential.Email != "" {
			return
		}
	}
}

func firstJSONString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := obj[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func (c Credential) Encode() ([]byte, error) {
	return json.Marshal(c)
}

func (c Credential) Ready() bool {
	return strings.TrimSpace(c.RefreshToken) != "" && strings.TrimSpace(c.AccountID) != ""
}

func ValidateCredential(payload []byte) error {
	credential, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(credential.RefreshToken) == "" && strings.TrimSpace(credential.AccessToken) == "" {
		return fmt.Errorf("codex credential requires refresh_token or access_token")
	}
	return nil
}

func (c Credential) needsRefresh(now time.Time) bool {
	if strings.TrimSpace(c.AccessToken) == "" {
		return true
	}
	if c.ExpiresAt <= 0 {
		return true
	}
	return now.Add(refreshLead).Unix() >= c.ExpiresAt
}

func unixSeconds(value int64) int64 {
	if value > 1e12 {
		return value / 1000
	}
	return value
}

// parseExpire accepts both Unix seconds and RFC3339-ish strings; CLIProxyAPI
// stores Expire as a string timestamp.
func parseExpire(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Unix()
	}
	var n int64
	if _, err := fmt.Sscanf(raw, "%d", &n); err == nil {
		return unixSeconds(n)
	}
	return 0
}
