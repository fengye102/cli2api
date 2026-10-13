package antigravity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/auth"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Browser login for Antigravity: Google's authorization-code + loopback round.
//
// The console opens the consent URL in a tab; Google redirects back to this
// process's own loopback listener, which swaps the code for tokens and stores
// the account. A pasted callback URL works too, for the case where the browser
// runs on a different machine than the gateway.
//
// There is no PKCE here (Google's desktop client does not require it for this
// flow), so `state` is the only one-shot credential on the wire: it is minted
// per round, echoed by the callback, and compared byte-for-byte before the code
// is spent. A stale link from an earlier round therefore cannot complete a newer
// session.
//
// The refresh token is the account's main credential, so a successful exchange
// that comes back without one is reported as a fixable 400 rather than stored:
// Google only issues it on the first consent, and repeating the authorization
// requires revoking the app in the Google account first.

const (
	// loginRoundTTL bounds how long an operator has to finish a login.
	loginRoundTTL = 10 * time.Minute
	// loopbackPath is the callback path; the redirect_uri echoes it verbatim.
	loopbackPath = "/oauth-callback"
)

// loginRound is one browser-login round for an account.
type loginRound struct {
	state     string
	redirect  string
	createdAt time.Time
	done      bool
	message   string
	err       error
}

// loginState mints the one-shot state value. It is the only unpredictable
// credential on this link, so it comes from crypto/rand.
func loginState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("antigravity login: no random source: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// loopbackListener binds the callback listener once per client and keeps it for
// the process lifetime: the redirect_uri must stay stable across rounds so a
// browser that lands on an old tab still reaches this process.
func (c *Client) loopbackListener() (net.Listener, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listener != nil {
		return c.listener, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("antigravity callback listen: %w", err)
	}
	c.listener = ln
	auth.ServeLoopback(ln, loopbackPath, "Antigravity", c.acceptCallback)
	return ln, nil
}

// redirectURI is the loopback callback URL the authorize URL and the code
// exchange must both use byte-for-byte.
func (c *Client) redirectURI() (string, error) {
	ln, err := c.loopbackListener()
	if err != nil {
		return "", err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return "", fmt.Errorf("antigravity callback address unavailable")
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", addr.Port, loopbackPath), nil
}

// StartLogin opens a round and returns the consent URL the console opens.
func (c *Client) StartLogin(ctx context.Context, accountID string) (providers.LoginSession, error) {
	state, err := loginState()
	if err != nil {
		return providers.LoginSession{}, err
	}
	redirect, err := c.redirectURI()
	if err != nil {
		return providers.LoginSession{}, err
	}
	c.setLoginRound(accountID, &loginRound{state: state, redirect: redirect, createdAt: time.Now()})
	return providers.LoginSession{
		AuthURL: buildAuthorizeURL(redirect, state),
		State:   state,
	}, nil
}

// PollLogin reports the round's state. The loopback listener completes the
// round on its own, so the poll only has to read what it recorded.
func (c *Client) PollLogin(ctx context.Context, accountID string) (bool, string, error) {
	round := c.loginRoundFor(accountID)
	if round == nil {
		return false, "", fmt.Errorf("login not started for account %s", accountID)
	}
	c.mu.Lock()
	done, message, failure := round.done, round.message, round.err
	c.mu.Unlock()
	if failure != nil {
		c.clearLoginRound(accountID)
		return false, "", failure
	}
	if done {
		// The round has served its purpose once reported; clearing it keeps a
		// later poll from claiming a login is still in flight.
		c.clearLoginRound(accountID)
		return true, message, nil
	}
	if time.Since(round.createdAt) > loginRoundTTL {
		c.clearLoginRound(accountID)
		return false, "", fmt.Errorf("登录已过期，请重新发起")
	}
	return false, "授权页已打开：在页面里完成 Google 登录即可，这个窗口会自己变成「已完成」。若浏览器在另一台机器上，可把回调链接粘贴到下面的输入框。", nil
}

// CompleteLogin finishes a round from a pasted callback URL.
func (c *Client) CompleteLogin(ctx context.Context, accountID, callbackURL string) error {
	round := c.loginRoundFor(accountID)
	if round == nil {
		return fmt.Errorf("login not started for account %s: start the browser login first", accountID)
	}
	if time.Since(round.createdAt) > loginRoundTTL {
		c.clearLoginRound(accountID)
		return fmt.Errorf("登录已过期，请重新发起")
	}
	parsed, err := url.Parse(strings.TrimSpace(callbackURL))
	if err != nil {
		return fmt.Errorf("parse callback url: %w", err)
	}
	query := parsed.Query()
	if failure := strings.TrimSpace(query.Get("error")); failure != "" {
		description := strings.TrimSpace(query.Get("error_description"))
		if description == "" {
			description = failure
		}
		return fmt.Errorf("Google 拒绝了这次授权：%s", description)
	}
	if got := strings.TrimSpace(query.Get("state")); got != round.state {
		return fmt.Errorf("回调里的 state 与本次登录不匹配，请重新发起网页登录")
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		return fmt.Errorf("回调链接里没有授权码，请重新发起网页登录")
	}
	credential, err := c.finishRound(ctx, code, round.redirect)
	if err != nil {
		return err
	}
	return c.storeOAuthCredential(ctx, accountID, credential)
}

// acceptCallback is the loopback handler: it validates the state, spends the
// code, and stores the credential.
func (c *Client) acceptCallback(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("parse callback url: %w", err)
	}
	query := parsed.Query()
	state := strings.TrimSpace(query.Get("state"))
	accountID, round := c.roundByState(state)
	if round == nil {
		return fmt.Errorf("回调里的 state 不属于任何进行中的登录，请重新发起网页登录")
	}
	if failure := strings.TrimSpace(query.Get("error")); failure != "" {
		description := strings.TrimSpace(query.Get("error_description"))
		if description == "" {
			description = failure
		}
		c.markRoundFailed(accountID, fmt.Errorf("Google 拒绝了这次授权：%s", description))
		return nil
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		c.markRoundFailed(accountID, fmt.Errorf("回调链接里没有授权码，请重新发起网页登录"))
		return nil
	}
	credential, err := c.finishRound(ctx, code, round.redirect)
	if err != nil {
		c.markRoundFailed(accountID, err)
		return err
	}
	if err := c.storeOAuthCredential(ctx, accountID, credential); err != nil {
		c.markRoundFailed(accountID, err)
		return err
	}
	c.markRoundDone(accountID)
	return nil
}

// finishRound exchanges the code, then enriches the credential with the email
// and project (both best effort — neither blocks storing the account).
func (c *Client) finishRound(ctx context.Context, code, redirectURI string) (Credential, error) {
	token, err := c.exchangeAuthorizationCode(ctx, code, redirectURI)
	if err != nil {
		return Credential{}, err
	}
	refreshToken := strings.TrimSpace(token.RefreshToken)
	if refreshToken == "" {
		return Credential{}, &providers.Error{
			Kind:   "auth",
			Status: 400,
			Message: "Google 未返回 refresh token（该授权此前完成过，重复授权不会再次下发）：" +
				"请在 Google 账号的「第三方访问 / 已授权的应用」里撤销本应用后重新登录",
		}
	}
	accessToken := strings.TrimSpace(token.AccessToken)
	credential := Credential{
		Format:       CredentialFormatOAuth,
		AuthMode:     AuthModeOAuth,
		RefreshToken: refreshToken,
		AccessToken:  accessToken,
		ExpiresAt:    token.expiresAtMillis(time.Now()),
	}
	if email := c.fetchEmail(ctx, accessToken); email != "" {
		credential.Email = email
	}
	if project, err := c.discoverProject(ctx, "", accessToken); err == nil {
		credential.ProjectID = project
	}
	return credential, nil
}

// storeOAuthCredential persists a freshly minted credential and marks the
// account ready. The loopback callback and the pasted-callback path both end
// here.
//
// The round is deliberately left in place: the console's poll reads it to learn
// that the login finished, and only then is it cleared.
func (c *Client) storeOAuthCredential(ctx context.Context, accountID string, credential Credential) error {
	payload, err := credential.Encode()
	if err != nil {
		return err
	}
	if err := c.store.SaveCredentialPayload(ctx, accountID, CredentialFormatOAuth, payload); err != nil {
		return err
	}
	_ = c.store.Observe(ctx, accountID, credential.Email, "ready", "", "")
	c.markRoundDone(accountID)
	return nil
}

// setLoginRound records the round for an account, replacing any earlier one.
func (c *Client) setLoginRound(accountID string, round *loginRound) {
	c.mu.Lock()
	if c.rounds == nil {
		c.rounds = map[string]*loginRound{}
	}
	c.rounds[accountID] = round
	c.mu.Unlock()
}

// loginRoundFor reads the account's round, if any.
func (c *Client) loginRoundFor(accountID string) *loginRound {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rounds[accountID]
}

// roundByState finds the round whose state matches, so the loopback callback can
// resolve which account it belongs to.
func (c *Client) roundByState(state string) (string, *loginRound) {
	if state == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for accountID, round := range c.rounds {
		if round.state == state {
			return accountID, round
		}
	}
	return "", nil
}

// clearLoginRound forgets the account's round.
func (c *Client) clearLoginRound(accountID string) {
	c.mu.Lock()
	delete(c.rounds, accountID)
	c.mu.Unlock()
}

// markRoundDone records a successful round (the poll reports it).
func (c *Client) markRoundDone(accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if round := c.rounds[accountID]; round != nil {
		round.done = true
		round.message = "登录完成"
	}
}

// markRoundFailed records a failed round (the poll reports it).
func (c *Client) markRoundFailed(accountID string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if round := c.rounds[accountID]; round != nil {
		round.err = err
	}
}

// roundState exposes the state of a round for tests and diagnostics.
func (c *Client) roundState(accountID string) string {
	if round := c.loginRoundFor(accountID); round != nil {
		return round.state
	}
	return ""
}
