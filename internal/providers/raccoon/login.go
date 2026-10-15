package raccoon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Browser login for Raccoon (小浣熊).
//
// The official web client opens a desktop-login round at
//
//	{main}/code/authorize?login_source=desktop&appname=<client>&state=<state>
//
// and the page then redirects to a *custom-scheme* callback,
//
//	office-raccoon://auth/callback?code=…&state=…
//
// which this gateway can never receive: only a desktop client that registered
// that scheme can. The console therefore always finishes this provider from a
// pasted callback URL — the same fallback trae / devin / codex / zcode use.
//
// ── Why the state is minted here and compared byte-for-byte ─────────────
// The callback is out of band, so anything able to observe or forge one could
// hand back a code that belongs to somebody else; the credential stored would
// then point at that other account. `state` is the only value that answers
// "was this callback produced by the round this process just started", so it is
// minted per round from crypto/rand, echoed by the page, and compared exactly
// before the code is spent. A stale link from an earlier round cannot complete
// a newer session.
//
// ── Where the credential comes from (the "PAT" question) ────────────────
// Raccoon has no personal access token. Its durable credential is the
// refresh/access token pair this exchange returns, or the same pair already
// sitting in the desktop client's shared login file
// (~/.box-agent/config/auth.json) which the import path accepts. Both routes
// end at the same Credential, so there is exactly one credential shape.

const (
	// loginRoundTTL bounds how long a pasted callback is still paired with the
	// round that minted its state.
	loginRoundTTL = 15 * time.Minute

	// authorizePath is the official desktop-login entry point.
	authorizePath = "/code/authorize"
	// loginSource marks the desktop branch of that page.
	loginSource = "desktop"
	// appName is the client name the page reports back. It is Chinese, so the
	// authorize URL must percent-encode it rather than paste it raw.
	appName = "办公小浣熊客户端"

	// exchangePath spends an authorization code for a token pair.
	exchangePath = "/login_with_authorization_code"

	// callbackScheme/Host/Path are the fixed callback shape the official page
	// redirects to. They are matched one by one, never by prefix: a prefix test
	// would also accept office-raccoon://auth/callback@evil.example or
	// office-raccoon://auth/callback.evil.
	callbackScheme = "office-raccoon"
	callbackHost   = "auth"
	callbackPath   = "/callback"

	// codeExpired is the upstream business code for "this code was already
	// spent, or sat around too long". It deserves its own message: retrying the
	// same link cannot help, the operator has to start a new round.
	codeExpired = 200035

	// maxStateLength / maxCodeLength cap what is echoed back, so an oversized
	// value is rejected outright instead of being compared against something it
	// could never equal.
	maxStateLength = 512
	maxCodeLength  = 8192
)

// loginRound is one browser-login round for an account.
type loginRound struct {
	state     string
	createdAt time.Time
	done      bool
	message   string
	err       error
}

// newLoginState mints the one-shot state value. It is the only unpredictable
// credential on this link, so it comes from crypto/rand.
func newLoginState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("raccoon login: no random source: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// mainOrigin is the site the authorize page lives on.
func (c *Client) mainOrigin() string {
	if origin := strings.TrimSpace(c.siteBase); origin != "" {
		return strings.TrimRight(origin, "/")
	}
	return siteBaseURL
}

// buildAuthorizeURL assembles the desktop-login URL. It uses url.Values rather
// than string concatenation because appName is Chinese and state is opaque:
// both must be percent-encoded, and a hand-built "&" would hand the page half
// an argument.
func (c *Client) buildAuthorizeURL(state string) string {
	query := url.Values{}
	query.Set("login_source", loginSource)
	query.Set("appname", appName)
	query.Set("state", state)
	return c.mainOrigin() + authorizePath + "?" + query.Encode()
}

// StartLogin opens a round and returns the URL the console opens.
func (c *Client) StartLogin(ctx context.Context, accountID string) (providers.LoginSession, error) {
	state, err := newLoginState()
	if err != nil {
		return providers.LoginSession{}, err
	}
	c.setLoginRound(accountID, &loginRound{state: state, createdAt: time.Now()})
	return providers.LoginSession{
		AuthURL: c.buildAuthorizeURL(state),
		State:   state,
	}, nil
}

// PollLogin reports the round's state. The callback arrives out of band, so the
// poll only ever reads what CompleteLogin recorded; it cannot finish the round
// on its own.
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
		return false, "", fmt.Errorf("登录已过期，请重新发起网页登录")
	}
	return false, "授权页已打开：登录成功后浏览器会跳到一个 office-raccoon:// 开头的链接（它只会唤起本机客户端，打不开也没关系）。把地址栏里的完整链接粘贴到下面的输入框即可完成。", nil
}

// CompleteLogin finishes a round from a pasted callback URL.
func (c *Client) CompleteLogin(ctx context.Context, accountID, callbackURL string) error {
	round := c.loginRoundFor(accountID)
	if round == nil {
		return fmt.Errorf("login not started for account %s: start the browser login first", accountID)
	}
	if time.Since(round.createdAt) > loginRoundTTL {
		c.clearLoginRound(accountID)
		return fmt.Errorf("登录已过期，请重新发起网页登录")
	}
	code, err := parseCallbackCode(callbackURL, round.state)
	if err != nil {
		return err
	}
	credential, err := c.exchangeAuthorizationCode(ctx, accountID, code)
	if err != nil {
		return err
	}
	return c.storeLoginCredential(ctx, accountID, credential)
}

// parseCallbackCode validates the callback shape and extracts the code, after
// checking that the state is the one this process minted.
func parseCallbackCode(rawURL, expectedState string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("回调地址无法解析，请粘贴完整的链接")
	}
	// The custom scheme goes through the opaque-host path, which does not fold
	// case the way http(s) does, so the host is compared case-insensitively.
	host := strings.ToLower(strings.TrimSpace(parsed.Host))
	matches := strings.EqualFold(parsed.Scheme, callbackScheme) &&
		host == callbackHost &&
		parsed.Path == callbackPath
	if !matches {
		return "", fmt.Errorf("回调地址不是 office-raccoon://auth/callback 形态，请粘贴授权后地址栏里的完整链接")
	}
	query := parsed.Query()
	if failure := strings.TrimSpace(query.Get("error")); failure != "" {
		description := strings.TrimSpace(query.Get("error_description"))
		if description == "" {
			description = failure
		}
		return "", fmt.Errorf("小浣熊拒绝了这次授权：%s", description)
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		return "", fmt.Errorf("回调链接里没有授权码，请重新发起网页登录")
	}
	if len(code) > maxCodeLength {
		return "", fmt.Errorf("授权码过长")
	}
	state := strings.TrimSpace(query.Get("state"))
	if state == "" {
		return "", fmt.Errorf("回调链接里没有 state，无法确认这次登录由本机发起，请重新发起网页登录")
	}
	if len(state) > maxStateLength {
		return "", fmt.Errorf("回调链接里的 state 过长")
	}
	if state != expectedState {
		return "", fmt.Errorf("回调里的 state 与本次登录不匹配，请重新发起网页登录")
	}
	return code, nil
}

// exchangeAuthorizationCode spends the code for a token pair. A code that has
// already been used (or sat too long) comes back as business code 200035 and is
// reported as "start over" rather than as a transport failure.
func (c *Client) exchangeAuthorizationCode(ctx context.Context, accountID, code string) (Credential, error) {
	body, err := json.Marshal(map[string]string{"authorization_code": code})
	if err != nil {
		return Credential{}, err
	}
	endpoint := strings.TrimRight(c.authBase, "/") + exchangePath
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Credential{}, fmt.Errorf("网页登录换取凭证失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &payload)
	if payload.Code == codeExpired {
		return Credential{}, fmt.Errorf("授权码已失效（可能重复粘贴或放置过久），请重新发起网页登录")
	}
	if resp.StatusCode >= 300 || payload.Code != 0 {
		message := firstNonEmpty(payload.Message, payload.Msg)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return Credential{}, fmt.Errorf("网页登录换取凭证失败：%s", message)
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
	}
	if err := json.Unmarshal(payload.Data, &data); err != nil {
		return Credential{}, fmt.Errorf("网页登录换取凭证失败：响应无法解析")
	}
	credential := Credential{
		Format:       CredentialFormat,
		AccessToken:  strings.TrimSpace(strings.TrimPrefix(data.AccessToken, "Bearer ")),
		RefreshToken: strings.TrimSpace(data.RefreshToken),
		UserID:       strings.TrimSpace(data.UserID),
	}
	if !credential.Ready() {
		return Credential{}, fmt.Errorf("网页登录没有返回可用凭证，请重新发起网页登录")
	}
	return credential, nil
}

// storeLoginCredential persists a freshly exchanged credential and marks the
// account ready. The round is deliberately left in place: the console's poll
// reads it to learn that the login finished, and only then is it cleared.
func (c *Client) storeLoginCredential(ctx context.Context, accountID string, credential Credential) error {
	payload, err := EncodeCredential(credential)
	if err != nil {
		return err
	}
	if err := c.store.SaveCredentialPayload(ctx, accountID, CredentialFormat, payload); err != nil {
		return err
	}
	_ = c.store.Observe(ctx, accountID, credential.UserID, "ready", "", "")
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
