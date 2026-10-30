package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// refreshFallbackCode is the upstream code for "signature check failed". The
// signed /refresh path rejects it; /agent-refresh accepts the same body.
const refreshFallbackCode = 400002

// credential loads the stored credential for one account, refreshing it when
// it is inside the expiry window.
func (c *Client) credential(ctx context.Context, accountID string) (Credential, error) {
	_, payload, err := c.store.LoadCredentialPayload(ctx, accountID)
	if err != nil {
		return Credential{}, &providers.Error{
			Kind:    accounts.KindAuth,
			Status:  http.StatusUnauthorized,
			Message: "autoclaw credential unavailable: " + err.Error(),
		}
	}
	cred, err := DecodeCredential(payload)
	if err != nil {
		return Credential{}, &providers.Error{
			Kind:    accounts.KindAuth,
			Status:  http.StatusUnauthorized,
			Message: err.Error(),
		}
	}
	if cred.Region == "" {
		cred.Region = c.region.String()
	}
	// Backfill expires_at for credentials stored before the importer derived
	// it. Without this an account imported as a bare token pair keeps an empty
	// expiry forever, NeedsRefresh() stays false, and the token dies silently
	// 24h later with no proactive renewal.
	if cred.ExpiresAt == "" {
		if expiry, ok := jwtExpiry(cred.AccessToken); ok {
			cred.ExpiresAt = expiry
		}
	}
	if !cred.NeedsRefresh(c.now()) {
		return cred, nil
	}
	return c.refreshCredential(ctx, accountID, cred)
}

// refreshCredential rotates the token, serialized per account so a burst of
// 401s cannot spend the single-use refresh token more than once.
func (c *Client) refreshCredential(ctx context.Context, accountID string, cred Credential) (Credential, error) {
	if !cred.CanRefresh() {
		return Credential{}, &providers.Error{
			Kind:    accounts.KindAuth,
			Status:  http.StatusUnauthorized,
			Message: "autoclaw credential has no refresh token; add the account again",
		}
	}
	lock := c.accountLock(accountID)
	lock.Lock()
	defer lock.Unlock()
	// Re-read under the lock: another request may have refreshed already.
	_, payload, err := c.store.LoadCredentialPayload(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	current, err := DecodeCredential(payload)
	if err != nil {
		return Credential{}, err
	}
	if current.AccessToken != cred.AccessToken {
		return current, nil
	}
	next, err := c.callRefresh(ctx, accountID, current)
	if err != nil {
		return Credential{}, err
	}
	if err := c.saveCredential(ctx, accountID, next); err != nil {
		// The refresh token is single-use: continuing with a credential only
		// this process knows about would lose it on restart.
		return Credential{}, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  http.StatusBadGateway,
			Message: "autoclaw refresh succeeded but could not be saved: " + err.Error(),
		}
	}
	c.markRefreshed(accountID)
	return next, nil
}

// callRefresh POSTs the refresh body, falling back to agent-refresh when the
// signed path rejects the signature.
func (c *Client) callRefresh(ctx context.Context, accountID string, cred Credential) (Credential, error) {
	body := map[string]any{
		"refresh_token": cred.RefreshToken,
		"source_id":     "autoclaw",
	}
	if cred.DeviceID != "" {
		body["device_id"] = cred.DeviceID
	}
	raw, status, err := c.doJSON(ctx, accountID, cred.AccessToken, refreshPath, body)
	if err != nil {
		return Credential{}, c.transportError(err)
	}
	env, parseErr := parseEnvelope(raw)
	if parseErr != nil {
		return Credential{}, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  http.StatusBadGateway,
			Message: parseErr.Error(),
		}
	}
	if env.Code == refreshFallbackCode {
		raw, status, err = c.doJSON(ctx, accountID, cred.AccessToken, refreshFallbackPath, body)
		if err != nil {
			return Credential{}, c.transportError(err)
		}
		env, parseErr = parseEnvelope(raw)
		if parseErr != nil {
			return Credential{}, &providers.Error{
				Kind:    accounts.KindUnavailable,
				Status:  http.StatusBadGateway,
				Message: parseErr.Error(),
			}
		}
	}
	if env.Code != 0 {
		return Credential{}, &providers.Error{
			Kind:    accounts.KindAuth,
			Status:  http.StatusUnauthorized,
			Message: fmt.Sprintf("autoclaw refresh rejected: code=%d msg=%s", env.Code, truncateText(env.Msg, 160)),
		}
	}
	var data struct {
		AccessToken       string `json:"access_token"`
		AccessTokenCamel  string `json:"accessToken"`
		RefreshToken      string `json:"refresh_token"`
		RefreshTokenCamel string `json:"refreshToken"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return Credential{}, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  http.StatusBadGateway,
			Message: "parse autoclaw refresh data: " + err.Error(),
		}
	}
	access := firstNonEmpty(data.AccessToken, data.AccessTokenCamel)
	if access == "" {
		return Credential{}, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  http.StatusBadGateway,
			Message: fmt.Sprintf("autoclaw refresh returned no access token (http=%d)", status),
		}
	}
	next := cred
	next.AccessToken = access
	// The upstream rotates the refresh token; keep the old one only when the
	// response omits it.
	next.RefreshToken = firstNonEmpty(data.RefreshToken, data.RefreshTokenCamel, cred.RefreshToken)
	next.Format = c.region.CredentialFormat()
	next.Region = c.region.String()
	// Recompute expiry from the NEW token's JWT exp rather than assuming a
	// duration; keep the old value when the claim cannot be read.
	if expiry, ok := jwtExpiry(access); ok {
		next.ExpiresAt = expiry
	}
	return next, nil
}

// saveCredential persists the rotated credential.
func (c *Client) saveCredential(ctx context.Context, accountID string, cred Credential) error {
	payload, err := EncodeCredential(cred)
	if err != nil {
		return err
	}
	return c.store.SaveCredentialPayload(ctx, accountID, cred.Format, payload)
}

// markRefreshed records when this account last refreshed, for the hourly
// maintenance pass.
func (c *Client) markRefreshed(accountID string) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.refreshed == nil {
		c.refreshed = map[string]time.Time{}
	}
	c.refreshed[accountID] = c.now()
}

// hourlyRefreshDue reports whether maintenance should refresh this account
// even though the token is not near expiry.
//
// The purpose is to keep the refresh token warm: the upstream rotates it, and
// a long-idle copy can be invalidated, forcing a full re-login. This belongs in
// maintenance only - doing it per request would refresh on every call.
func (c *Client) hourlyRefreshDue(accountID string) bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	last, ok := c.refreshed[accountID]
	if !ok {
		return true
	}
	return c.now().Sub(last) >= hourlyForcedRefresh*time.Second
}

// jwtExpiry reads the exp claim out of a JWT without verifying it. The token is
// issued by the upstream we already trust; this only schedules refreshes.
func jwtExpiry(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := decodeJWTPart(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return "", false
	}
	return time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339), true
}

// jwtUserID reads a stable user id from the token claims for console display.
func jwtUserID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := decodeJWTPart(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		UserID    string `json:"user_id"`
		UserIDAlt string `json:"userId"`
		Sub       string `json:"sub"`
		UID       string `json:"uid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return firstNonEmpty(claims.UserID, claims.UserIDAlt, claims.UID, claims.Sub)
}

func decodeJWTPart(part string) ([]byte, error) {
	// RawURLEncoding with padding stripped is what JWT uses.
	return base64RawURLDecode(part)
}

// transportError wraps a network failure so the executor can classify it as an
// unreachable upstream (retry elsewhere, no cooldown) rather than a client
// error.
func (c *Client) transportError(err error) error {
	return &providers.Error{
		Kind:    accounts.KindUnavailable,
		Status:  http.StatusBadGateway,
		Message: "autoclaw upstream unreachable: " + err.Error(),
	}
}

func truncateText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
