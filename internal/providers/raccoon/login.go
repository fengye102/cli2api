package raccoon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/auth"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Browser login for Raccoon (小浣熊).
//
// ── Two callback modes upstream, and why only one works here ────────────
// The authorize page (`/authorize`) picks its callback from `login_source`:
//
//	desktop  → always redirects to the custom scheme office-raccoon://auth/callback
//	           (verified in the page bundle: desktop ? "office-raccoon://auth/callback" : e.redirect)
//	other    → redirects to the `redirect` parameter
//
// Desktop mode is useless to a gateway: the browser hands the URL to a
// registered *desktop client* instead of showing it, so nothing appears in the
// address bar for an operator to copy, and the callback never reaches us. That
// is why this provider does NOT use login_source=desktop.
//
// The page also constrains `redirect` — it must be a loopback http URL *with an
// explicit port* (127.0.0.1 / localhost / [::1]), or a vscode: scheme. A public
// https callback is rejected outright (verified: the page's parsed context comes
// back null). So the redirect points at this process's own loopback listener:
//
//   - if the browser can reach that port (same host, or a tunnel), the round
//     completes by itself, and
//   - otherwise the callback URL is delivered into a popup window, whose address
//     bar shows the full http://127.0.0.1:<port>/…?authorization_code=…&state=…
//     link for the operator to paste back. That is the "callback address" the
//     desktop mode never produced.
//
// ── Two parameter names ────────────────────────────────────────────────
// The page sets `authorization_code` for the non-desktop (loopback) callback but
// `code` for the desktop custom-scheme one. Both spellings are accepted so a
// pasted link from either mode imports.
//
// ── Why the state is minted here and compared byte-for-byte ─────────────
// The callback is out of band, so anything able to observe or forge one could
// hand back a code that belongs to somebody else; the credential stored would
// then point at that other account. `state` is the only value that answers "was
// this callback produced by the round this process just started", so it is
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

	// authorizePath is the page that hosts the consent UI. The older
	// `/code/authorize` form only redirects here, so it is skipped.
	authorizePath = "/authorize"

	// appName is the label the consent page shows. A gateway name keeps the copy
	// honest instead of naming a desktop client this flow does not use.
	appName = "CLI2API 网关"

	// callbackPath is the loopback callback path. The redirect_uri echoes it
	// verbatim, so it must stay stable across rounds.
	callbackPath = "/raccoon-callback"

	// exchangePath spends an authorization code for a token pair.
	exchangePath = "/login_with_authorization_code"

	// desktopScheme/Host/Path are the *legacy* desktop callback shape. It is
	// still accepted so an operator holding a link produced by the desktop
	// client can import it, but it is never used to start a round.
	//
	// They are matched one by one, never by prefix: a prefix test would also
	// accept office-raccoon://auth/callback@evil.example or
	// office-raccoon://auth/callback.evil.
	desktopScheme = "office-raccoon"
	desktopHost   = "auth"
	desktopPath   = "/callback"

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
	redirect  string
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

// loopbackListener binds the callback listener once per client and keeps it for
// the process lifetime: the redirect_uri must stay stable across rounds so a
// browser that lands on an old popup still reaches this process.
func (c *Client) loopbackListener() (net.Listener, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listener != nil {
		return c.listener, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("raccoon callback listen: %w", err)
	}
	c.listener = ln
	auth.ServeLoopback(ln, callbackPath, "Raccoon", c.acceptCallback)
	return ln, nil
}

// redirectURI is the loopback callback URL the authorize URL must carry. The
// page requires an explicit port, which the listener always has.
func (c *Client) redirectURI() (string, error) {
	ln, err := c.loopbackListener()
	if err != nil {
		return "", err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return "", fmt.Errorf("raccoon callback address unavailable")
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", addr.Port, callbackPath), nil
}

// buildAuthorizeURL assembles the consent URL. It uses url.Values rather than
// string concatenation because appName is Chinese and the redirect/state are
// opaque: all must be percent-encoded, and a hand-built "&" would hand the page
// half an argument.
//
// login_source is deliberately omitted: the page rejects any value other than
// "desktop", and "desktop" would send the callback to the custom scheme instead
// of the loopback URL this gateway can receive.
func (c *Client) buildAuthorizeURL(state, redirect string) string {
	query := url.Values{}
	query.Set("appname", appName)
	query.Set("state", state)
	query.Set("redirect", redirect)
	return c.mainOrigin() + authorizePath + "?" + query.Encode()
}

// StartLogin opens a round and returns the URL the console opens.
func (c *Client) StartLogin(ctx context.Context, accountID string) (providers.LoginSession, error) {
	state, err := newLoginState()
	if err != nil {
		return providers.LoginSession{}, err
	}
	redirect, err := c.redirectURI()
	if err != nil {
		return providers.LoginSession{}, err
	}
	c.setLoginRound(accountID, &loginRound{state: state, redirect: redirect, createdAt: time.Now()})
	return providers.LoginSession{
		AuthURL: c.buildAuthorizeURL(state, redirect),
		State:   state,
	}, nil
}

// PollLogin reports the round's state. The loopback listener completes the round
// on its own when it is reachable; otherwise the operator finishes it from the
// pasted callback URL, so the poll only reads what was recorded.
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
	return false, "授权页已打开：登录并确认授权后，浏览器会弹出一个新窗口，地址栏里是 http://127.0.0.1:…/raccoon-callback?authorization_code=…&state=… 形式的链接（该窗口本身打不开是正常的，回调端口在服务器上）。把那个完整链接复制粘贴到下面的输入框即可完成。", nil
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

// acceptCallback is the loopback handler. It receives a *relative* URL (the
// listener's own path plus query), so it reads the query directly: the listener
// already guarantees the shape, and only the state needs checking.
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
		authErr := fmt.Errorf("小浣熊拒绝了这次授权：%s", description)
		c.markRoundFailed(accountID, authErr)
		return nil
	}
	code, err := codeFromQuery(query)
	if err != nil {
		c.markRoundFailed(accountID, err)
		return nil
	}
	credential, err := c.exchangeAuthorizationCode(ctx, accountID, code)
	if err != nil {
		c.markRoundFailed(accountID, err)
		return err
	}
	if err := c.storeLoginCredential(ctx, accountID, credential); err != nil {
		c.markRoundFailed(accountID, err)
		return err
	}
	return nil
}

// parseCallbackCode validates a pasted callback URL and extracts the code, after
// checking that the state is the one this process minted.
//
// Two shapes are accepted: the loopback http URL this gateway asks for, and the
// legacy desktop custom scheme (so a link produced by the desktop client still
// imports). Each is matched field by field rather than by prefix.
func parseCallbackCode(rawURL, expectedState string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("回调地址无法解析，请粘贴完整的链接")
	}
	query := parsed.Query()
	if failure := strings.TrimSpace(query.Get("error")); failure != "" {
		description := strings.TrimSpace(query.Get("error_description"))
		if description == "" {
			description = failure
		}
		return "", fmt.Errorf("小浣熊拒绝了这次授权：%s", description)
	}
	if !isLoopbackCallback(parsed) && !isDesktopCallback(parsed) {
		return "", fmt.Errorf("回调地址不是 http://127.0.0.1:<端口>/raccoon-callback 形态（也不像桌面端的 office-raccoon://auth/callback），请粘贴授权后弹出的那个窗口地址栏里的完整链接")
	}
	code, err := codeFromQuery(query)
	if err != nil {
		return "", err
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

// codeFromQuery reads the authorization code under either upstream spelling.
func codeFromQuery(query url.Values) (string, error) {
	// Non-desktop (loopback) callbacks use authorization_code; the desktop
	// custom-scheme one uses code. Accept both, preferring the loopback name.
	code := strings.TrimSpace(query.Get("authorization_code"))
	if code == "" {
		code = strings.TrimSpace(query.Get("code"))
	}
	if code == "" {
		return "", fmt.Errorf("回调链接里没有授权码，请重新发起网页登录")
	}
	if len(code) > maxCodeLength {
		return "", fmt.Errorf("授权码过长")
	}
	return code, nil
}

// isLoopbackCallback reports whether the URL is the http loopback callback this
// gateway asks the page to use. The port must be present: the page rejects a
// loopback redirect without one, so a URL missing it cannot be a real callback.
func isLoopbackCallback(parsed *url.URL) bool {
	if !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	if parsed.Path != callbackPath {
		return false
	}
	if parsed.Port() == "" {
		return false
	}
	// The page allows exactly these three hostnames.
	switch strings.ToLower(parsed.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// isDesktopCallback reports whether the URL is the legacy desktop custom-scheme
// callback. The custom scheme goes through the opaque-host path, which does not
// fold case the way http(s) does, so the host is compared case-insensitively.
func isDesktopCallback(parsed *url.URL) bool {
	return strings.EqualFold(parsed.Scheme, desktopScheme) &&
		strings.EqualFold(strings.TrimSpace(parsed.Host), desktopHost) &&
		parsed.Path == desktopPath
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
