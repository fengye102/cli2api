// Package workbuddy implements the WorkBuddy / CodeBuddy in-process provider.
// Protocol constants live only in this package.
package workbuddy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

const (
	CredentialFormat = "workbuddy-oauth-v1"

	DomainCN     = "codebuddy.cn"
	DomainGlobal = "workbuddy.ai"

	ChatBaseCN     = "https://copilot.tencent.com"
	ChatBaseGlobal = "https://www.workbuddy.ai"
	BillingBaseCN  = "https://www.codebuddy.cn"

	// UserAgent matches official @tencent-ai/codebuddy-code 2.139.0 in the
	// dual-token form WorkBuddy chat still accepts. Origin/Referer stay
	// region-specific and must never mix CN with Global.
	CLIVersion = "2.139.0"
	UserAgent  = "CLI/" + CLIVersion + " CodeBuddy/" + CLIVersion

	// DesktopUserAgent is for /v3/config only. Live A/B showed the same
	// Global account returns the IDE dropdown (incl. deepseek-v4.1-flash)
	// with a desktop UA, but the CLI UA returns a different model set.
	DesktopVersion   = "5.4.2"
	DesktopUserAgent = "WorkBuddy/" + DesktopVersion

	productTypeCLI     = "CLI"
	agentIntentDefault = "craft"
	agentTypeMain      = "main"

	pathAuthState    = "/v2/plugin/auth/state"
	pathAuthToken    = "/v2/plugin/auth/token"
	pathAuthAccount  = "/v2/plugin/login/account"
	pathTokenRefresh = "/v2/plugin/auth/token/refresh"
	pathChat         = "/v2/chat/completions"
	// CN still serves the console catalog to Bearer tokens. Global's
	// /console/enterprises/personal/models is an OIDC page: unauthenticated
	// 302 to Keycloak, authenticated 500 HTML. The plugin JSON catalog is
	// /v2/enterprises/personal/models. The IDE model dropdown is not that
	// catalog: desktop loads authenticated GET /v3/config (product config).
	pathModelsCN      = "/console/enterprises/personal/models"
	pathModelsGlobal  = "/v2/enterprises/personal/models"
	pathProductConfig = "/v3/config"
	pathUserResource  = "/v2/billing/meter/get-user-resource"
	pathDailyCheckin  = "/v2/billing/meter/daily-checkin"

	sessionDeadCode         = 12153
	sessionDeadText         = "Offline user session not found"
	missingSystemPromptCode = 11128
	missingSystemPromptText = "first message is not system prompt"
	toolCallSequenceCode    = 11148
	toolCallSequenceText    = "tool calls and tool results do not match"

	// rateLimitCode marks a usage limit whose response carries the absolute
	// reset timestamp. Cooling down for the generic rate-limit fallback would
	// retry a few seconds later and burn further quota.
	rateLimitCode = 6004
)

// quotaResetLocation is the fixed zone WorkBuddy uses for the reset timestamp
// in its CN usage-limit message ("... UTC+8 重置"). It is a var because
// time.FixedZone returns a *time.Location, which cannot be a constant.
var quotaResetLocation = time.FixedZone("UTC+8", 8*60*60)

// Credential is the canonical storage payload shape.
type Credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Nickname     string `json:"nickname"`
}

// DecodeCredential accepts both the canonical flat payload and the nested
// {account, auth} export shape.
func DecodeCredential(payload []byte) (Credential, error) {
	var nested struct {
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(payload, &nested); err == nil &&
		(nested.Auth.AccessToken != "" || nested.Account.UID != "") {
		return Credential{
			AccessToken:  nested.Auth.AccessToken,
			RefreshToken: nested.Auth.RefreshToken,
			ExpiresAt:    nested.Auth.ExpiresAt,
			Domain:       nested.Auth.Domain,
			UID:          nested.Account.UID,
			EnterpriseID: nested.Account.EnterpriseID,
			Nickname:     nested.Account.Nickname,
		}, nil
	}
	var flat struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
		Domain       string `json:"domain"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterprise_id"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(payload, &flat); err == nil && flat.AccessToken != "" {
		return Credential{
			AccessToken:  flat.AccessToken,
			RefreshToken: flat.RefreshToken,
			ExpiresAt:    flat.ExpiresAt,
			Domain:       flat.Domain,
			UID:          flat.UID,
			EnterpriseID: flat.EnterpriseID,
			Nickname:     flat.Nickname,
		}, nil
	}
	// Also accept flat upstream-style camelCase keys.
	var camel struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(payload, &camel); err == nil && camel.AccessToken != "" {
		return Credential(camel), nil
	}
	// Raw CodeBuddy state.vscdb auth value: nested auth/session blobs and
	// whole objects serialized as JSON strings, including the "uid+token"
	// packed token format (mirrors cockpit-tools' codebuddy_account.rs).
	if value, err := providers.UnwrapJSONValue(payload); err == nil {
		if credential, ok := credentialFromVscdbValue(value); ok {
			return credential, nil
		}
	}
	return Credential{}, fmt.Errorf("workbuddy credential requires access_token")
}

// credentialFromVscdbValue extracts a credential from the CodeBuddy desktop
// auth blob: the outermost object that carries an access token wins; uid
// comes from account/root fields or the "uid+token" packed form.
func credentialFromVscdbValue(value any) (Credential, bool) {
	auth := providers.DeepFindAuthObject(value)
	if auth == nil {
		return Credential{}, false
	}
	access := pickLocalField(auth, "accessToken", "access_token", "token")
	uid, access := splitPackedToken(access)
	if access == "" {
		return Credential{}, false
	}
	credential := Credential{
		AccessToken:  access,
		RefreshToken: pickLocalField(auth, "refreshToken", "refresh_token"),
		ExpiresAt:    unixSeconds(providers.DeepPickInt(value, "expiresAt", "expires_at")),
		Domain:       providers.DeepPickString(value, "domain"),
		Nickname:     providers.DeepPickString(value, "nickname", "name", "label"),
		EnterpriseID: providers.DeepPickString(value, "enterpriseId", "enterprise_id"),
	}
	if uid == "" {
		uid = providers.DeepPickString(value, "uid")
	}
	credential.UID = uid
	return credential, true
}

// pickLocalField reads a field from the auth blob itself or one level down
// (auth.accessToken / session patterns).
func pickLocalField(auth map[string]any, names ...string) string {
	for _, name := range names {
		if s, ok := auth[name].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	for _, seg := range []string{"auth", "session", "data"} {
		if inner, ok := auth[seg].(map[string]any); ok {
			for _, name := range names {
				if s, ok := inner[name].(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

// splitPackedToken handles CodeBuddy's local token format "uid+token".
func splitPackedToken(token string) (uid string, value string) {
	token = strings.TrimSpace(token)
	prefix, suffix, found := strings.Cut(token, "+")
	if !found || strings.TrimSpace(suffix) == "" {
		return "", token
	}
	return strings.TrimSpace(prefix), strings.TrimSpace(suffix)
}

func (c Credential) Encode() ([]byte, error) {
	return json.Marshal(c)
}

func (c Credential) Ready() bool {
	return strings.TrimSpace(c.AccessToken) != "" && strings.TrimSpace(c.UID) != ""
}

func (c Credential) ChatBase() string {
	if c.IsGlobal() {
		return ChatBaseGlobal
	}
	return ChatBaseCN
}

// productConfigPath is the IDE dropdown source (CloudProductManager /v3/config).
func (c Credential) productConfigPath() string {
	return pathProductConfig
}

func isCLIAgent(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cli", "codebuddy", "workbuddy":
		return true
	default:
		return false
	}
}

func (c Credential) BillingBase() string {
	if c.IsGlobal() {
		return ChatBaseGlobal
	}
	return BillingBaseCN
}

// ValidateCredential enforces the import contract: token plus uid are required
// for an account to be ready. A missing uid is stored but never ready.
func ValidateCredential(payload []byte) error {
	credential, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return fmt.Errorf("workbuddy credential requires accessToken")
	}
	return nil
}

func (c Credential) IsGlobal() bool {
	domain := strings.ToLower(strings.TrimSpace(c.Domain))
	if domain == "" {
		return false
	}
	if strings.Contains(domain, DomainCN) {
		return false
	}
	return strings.Contains(domain, DomainGlobal) || strings.Contains(domain, "workbuddy")
}

func unixSeconds(value int64) int64 {
	if value > 1e12 {
		return value / 1000
	}
	return value
}
