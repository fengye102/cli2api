package zcode

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

// Browser login for the domestic (BigModel) service, the only region this
// channel ships. The console asks for a session, opens the BigModel authorize
// URL in a browser tab, and the operator pastes the zcode:// callback URL
// back: the callback uses a custom scheme that only the official desktop
// client can receive, so no server-side loopback redirect can ever complete
// it, and pasting the callback is the whole flow rather than a fallback.
const (
	loginAuthorizeURL   = "https://bigmodel.cn/login"
	loginAuthorizeAppID = "zcode"
	loginRedirectURI    = "zcode://oauth/callback"
	loginTokenURL       = "https://zcode.z.ai/api/v1/oauth/token"
	bigmodelUserInfoURL = "https://open.bigmodel.cn/api/biz/customer/getCustomerInfo"

	// loginPendingTTL bounds how long a pasted callback is still paired with
	// the state it was minted for.
	loginPendingTTL = 15 * time.Minute
	// loginExchangeTimeout bounds the token exchange so a stuck upstream
	// cannot hold the console request open.
	loginExchangeTimeout = 30 * time.Second
)

// loginPending is one browser-login round for an account: the state we minted
// and when. The callback must echo that state, so a stale link from an
// earlier round cannot complete a newer session.
type loginPending struct {
	state     string
	createdAt time.Time
}

// StartLogin mints a state and returns the BigModel authorize URL the console
// opens in a browser tab.
func (c *Client) StartLogin(_ context.Context, accountID string) (providers.LoginSession, error) {
	state, err := loginState()
	if err != nil {
		return providers.LoginSession{}, err
	}
	c.mu.Lock()
	if c.pending == nil {
		c.pending = map[string]*loginPending{}
	}
	c.pending[accountID] = &loginPending{state: state, createdAt: time.Now()}
	c.mu.Unlock()
	return providers.LoginSession{AuthURL: buildAuthorizeURL(state), State: state}, nil
}

// PollLogin reports the state of a pasted-callback login. The browser cannot
// deliver a zcode:// callback to a server, so there is nothing to poll: the
// message tells the operator what to paste.
func (c *Client) PollLogin(_ context.Context, accountID string) (bool, string, error) {
	c.mu.Lock()
	pending := c.pending[accountID]
	c.mu.Unlock()
	if pending == nil {
		return false, "", fmt.Errorf("login not started for account %s", accountID)
	}
	if time.Since(pending.createdAt) > loginPendingTTL {
		c.clearLoginPending(accountID)
		return false, "", fmt.Errorf("login expired; start again")
	}
	return false, "在打开的页面完成登录后浏览器会跳到 zcode://oauth/callback?...（不会自动回到这里），把地址栏那条完整链接粘贴到下面的输入框", nil
}

// CompleteLogin exchanges the pasted callback URL's authorization code for
// tokens and stores the resulting credential on the account.
func (c *Client) CompleteLogin(ctx context.Context, accountID, callbackURL string) error {
	c.mu.Lock()
	pending := c.pending[accountID]
	c.mu.Unlock()
	if pending == nil {
		return fmt.Errorf("login not started for account %s: use the browser login first", accountID)
	}
	if time.Since(pending.createdAt) > loginPendingTTL {
		c.clearLoginPending(accountID)
		return fmt.Errorf("login expired; start again")
	}
	code, err := parseCallbackURL(callbackURL, pending.state)
	if err != nil {
		return err
	}
	credential, err := c.exchangeAuthorizationCode(ctx, code, pending.state)
	if err != nil {
		return err
	}
	payload, err := credential.Encode()
	if err != nil {
		return err
	}
	if err := c.store.SaveCredentialPayload(ctx, accountID, CredentialFormat, payload); err != nil {
		return err
	}
	_ = c.store.Observe(ctx, accountID, credential.UserID, "ready", "", "")
	c.clearLoginPending(accountID)
	return nil
}

func (c *Client) clearLoginPending(accountID string) {
	c.mu.Lock()
	delete(c.pending, accountID)
	c.mu.Unlock()
}

// loginState mints the anti-CSRF state echoed through the browser round trip.
func loginState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("zcode login state: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// buildAuthorizeURL builds the BigModel authorize URL. The parameter names
// (redirect / appId / state) are the ones the official client uses and the
// service validates; renaming them breaks the round trip.
func buildAuthorizeURL(state string) string {
	query := url.Values{}
	query.Set("redirect", loginRedirectURI)
	query.Set("appId", loginAuthorizeAppID)
	query.Set("state", state)
	return loginAuthorizeURL + "?" + query.Encode()
}

// parseCallbackURL validates the pasted zcode://oauth/callback URL and returns
// its authorization code. The state must match the round the console started,
// so a link from an older login cannot be replayed here.
func parseCallbackURL(raw, expectedState string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("paste the zcode://oauth/callback URL from the browser address bar")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid callback URL: %v", err)
	}
	if !strings.EqualFold(parsed.Scheme, "zcode") {
		return "", fmt.Errorf("callback must be a zcode:// URL (the address-bar link), got %q", clip(trimmed, 40))
	}
	host := strings.ToLower(parsed.Host)
	if host != "oauth" || !strings.HasPrefix(parsed.Path, "/callback") {
		return "", fmt.Errorf("unexpected callback target %q, want zcode://oauth/callback", parsed.Host+parsed.Path)
	}
	query := parsed.Query()
	if failure := strings.TrimSpace(query.Get("error")); failure != "" {
		return "", fmt.Errorf("authorization was rejected: %s", failure)
	}
	state := strings.TrimSpace(query.Get("state"))
	if state == "" {
		return "", fmt.Errorf("callback URL carries no state")
	}
	if expectedState != "" && state != expectedState {
		return "", fmt.Errorf("state mismatch (the link belongs to an earlier login); start the browser login again")
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		code = strings.TrimSpace(query.Get("authCode"))
	}
	if code == "" {
		return "", fmt.Errorf("callback URL carries no code")
	}
	return code, nil
}

// exchangeAuthorizationCode posts the authorization code to the ZCode token
// endpoint and turns the response into a stored credential. The response
// spellings follow spec §1.4: data.token is the ZCode JWT, data.bigmodel
// carries the provider access/refresh tokens.
func (c *Client) exchangeAuthorizationCode(ctx context.Context, code, state string) (Credential, error) {
	body, err := json.Marshal(map[string]string{
		"provider":     RegionBigModel,
		"code":         code,
		"redirect_uri": loginRedirectURI,
		"state":        state,
	})
	if err != nil {
		return Credential{}, err
	}
	endpoint := strings.TrimSpace(c.tokenURL)
	if endpoint == "" {
		endpoint = loginTokenURL
	}
	ctx, cancel := context.WithTimeout(ctx, loginExchangeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Credential{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Credential{}, fmt.Errorf("token exchange failed: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Credential{}, fmt.Errorf("token exchange failed: %w", err)
	}
	if response.StatusCode >= 300 {
		return Credential{}, fmt.Errorf("token exchange failed: HTTP %d %s", response.StatusCode, clip(strings.TrimSpace(string(payload)), 200))
	}
	var envelope struct {
		Code *int   `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token        string `json:"token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
			BigModel     struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
			} `json:"bigmodel"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return Credential{}, fmt.Errorf("token exchange returned an unreadable body: %v", err)
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return Credential{}, fmt.Errorf("token exchange rejected: %s", firstNonEmptyString(envelope.Msg, fmt.Sprintf("code %d", *envelope.Code)))
	}
	jwt := strings.TrimSpace(envelope.Data.Token)
	if jwt == "" {
		return Credential{}, fmt.Errorf("token exchange response carries no data.token (ZCode JWT)")
	}
	credential := Credential{
		Format:       CredentialFormat,
		AuthMode:     AuthModeOAuth,
		Provider:     RegionBigModel,
		ZCodeJWT:     jwt,
		AccessToken:  firstNonEmptyString(strings.TrimSpace(envelope.Data.BigModel.AccessToken), strings.TrimSpace(envelope.Data.AccessToken)),
		RefreshToken: firstNonEmptyString(strings.TrimSpace(envelope.Data.BigModel.RefreshToken), strings.TrimSpace(envelope.Data.RefreshToken)),
	}
	if envelope.Data.ExpiresIn > 0 {
		credential.ExpiresAt = time.Now().Add(time.Duration(envelope.Data.ExpiresIn) * time.Second).Unix()
	}
	fillJWTIdentity(&credential, jwt)
	if credential.AccessToken != "" {
		email, userID := c.domesticIdentity(ctx, credential.AccessToken)
		if credential.Email == "" {
			credential.Email = email
		}
		if credential.UserID == "" {
			credential.UserID = userID
		}
	}
	return credential, nil
}

// domesticIdentity reads the account identity from the BigModel customer
// endpoint. It is best-effort: login succeeds on the tokens alone, and the
// endpoint only fills the console's email / user id labels.
func (c *Client) domesticIdentity(ctx context.Context, accessToken string) (string, string) {
	endpoint := strings.TrimSpace(c.userInfoURL)
	if endpoint == "" {
		endpoint = bigmodelUserInfoURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", ""
	}
	request.Header.Set("Accept", "application/json")
	// The official client sends the raw access token here, without the Bearer
	// prefix; keep that spelling.
	request.Header.Set("Authorization", accessToken)
	response, err := c.http.Do(request)
	if err != nil {
		return "", ""
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode >= 300 {
		return "", ""
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", ""
	}
	fields := envelope.Data
	if len(fields) == 0 {
		return "", ""
	}
	email := pickLocalField(fields, "email", "userEmail")
	userID := pickLocalField(fields, "user_id", "userId", "customerNumber", "sub", "id")
	return email, userID
}

// clip shortens a value for error messages so a pasted blob cannot flood the
// console.
func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
