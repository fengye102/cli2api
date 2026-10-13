package antigravity

import (
	"context"
	"strings"
	"time"
)

// Token renewal for OAuth accounts.
//
// The access token lives about an hour, so it is renewed ahead of expiry and
// again on demand after a 401. Renewal is single-flight per account: concurrent
// requests wait on one round instead of each burning a refresh call. The result
// is written back only when the stored credential is still the one the refresh
// started from — the operator may have re-logged-in meanwhile, and an
// unconditional write would clobber the newer credential.

// refreshSkewMillis is the early-renewal window: renewal starts 15 minutes
// before expiry.
const refreshSkewMillis = 900 * 1000

// freshCredential returns a credential whose access token is valid for use. A
// key-mode credential is returned as-is; an OAuth one is renewed when the cached
// token is missing or inside the expiry window.
func (c *Client) freshCredential(ctx context.Context, accountID string) (Credential, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	if !cred.IsOAuth() {
		return cred, nil
	}
	if !cred.accessTokenExpiring() {
		return cred, nil
	}
	return c.renewCredential(ctx, accountID, cred)
}

// accessTokenExpiring reports whether the cached access token is missing or
// close enough to expiry that a renewal should happen now. An unknown expiry
// (ExpiresAt <= 0) reports false: that account is rescued by the 401 path
// instead of by renewing on every call.
func (c Credential) accessTokenExpiring() bool {
	if strings.TrimSpace(c.AccessToken) == "" {
		return true
	}
	if c.ExpiresAt <= 0 {
		return false
	}
	return c.ExpiresAt-refreshSkewMillis <= time.Now().UnixMilli()
}

// renewCredential refreshes an OAuth access token, deduping concurrent callers.
func (c *Client) renewCredential(ctx context.Context, accountID string, cred Credential) (Credential, error) {
	c.mu.Lock()
	if flight, ok := c.refreshing[accountID]; ok {
		c.mu.Unlock()
		select {
		case <-flight.done:
			return flight.cred, flight.err
		case <-ctx.Done():
			return Credential{}, ctx.Err()
		}
	}
	flight := &refreshFlight{done: make(chan struct{})}
	c.refreshing[accountID] = flight
	c.mu.Unlock()

	fresh, err := c.refreshAndStore(ctx, accountID, cred)

	c.mu.Lock()
	flight.cred, flight.err = fresh, err
	delete(c.refreshing, accountID)
	close(flight.done)
	c.mu.Unlock()
	return fresh, err
}

// refreshAndStore performs one refresh round and persists the outcome.
func (c *Client) refreshAndStore(ctx context.Context, accountID string, cred Credential) (Credential, error) {
	response, err := c.refreshAccessToken(ctx, accountID, cred.RefreshToken)
	if err != nil {
		return Credential{}, err
	}
	fresh := cred
	fresh.AccessToken = response.AccessToken
	if strings.TrimSpace(response.RefreshToken) != "" {
		// Google usually omits the refresh token on renewal; when it does return
		// one, take it. Otherwise the stored value stands.
		fresh.RefreshToken = response.RefreshToken
	}
	fresh.ExpiresAt = response.expiresAtMillis(time.Now())
	if strings.TrimSpace(fresh.ProjectID) == "" {
		if project, err := c.discoverProject(ctx, accountID, fresh.AccessToken); err == nil {
			fresh.ProjectID = project
		}
	}
	// Compare-before-write: only overwrite when the stored credential is still
	// the one this refresh began from.
	if current, err := c.credential(ctx, accountID); err == nil && current.RefreshToken == cred.RefreshToken {
		if payload, err := fresh.Encode(); err == nil {
			_ = c.store.SaveCredentialPayload(ctx, accountID, CredentialFormatOAuth, payload)
		}
	}
	return fresh, nil
}

// forceRenew renews a credential after an upstream 401, ignoring the expiry
// window: the cached token is evidently no longer accepted.
func (c *Client) forceRenew(ctx context.Context, accountID string) (Credential, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	if !cred.IsOAuth() {
		return cred, nil
	}
	return c.renewCredential(ctx, accountID, cred)
}
