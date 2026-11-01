package autoclaw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Account-side login. Until this existed a credential could only arrive by
// pasting the desktop client's auth.json - which, on a Windows install, holds a
// DPAPI blob that no server can open (see dpapi.go), so the channel was hostage
// to the desktop app: log out there and the gateway died with it.
//
// The account plane has its own login, and the two builds use different ones:
//
//   - CN (zhipuai): phone + SMS code. POST /userapi/v1/agent-send-code, then
//     POST /userapi/v1/agent-login/ - note the trailing slash, and note that
//     the code must be sent as a NUMBER; a string is rejected with 400001.
//   - Intl (autoglm.ai): the desktop client's OAuth round.
//     POST /userapi/overseasv1/{provider}-oauth-url  -> authorize URL
//     browser signs in, provider 302s back to http://localhost:<port>/auth/callback-{provider}?code&state
//     POST /userapi/overseasv1/{provider}-oauth-login {code,state,navigate_uri} -> tokens
//     Only zai (chatglm.cn) and google are registered providers, the callback
//     ports are whitelisted (18432 first), and navigate_uri has to be one of
//     them or the gateway answers 400001.
//
// Both rounds are interactive, and this process is not on the operator's
// machine, so the callback never arrives on a socket here: the console shows
// the authorize URL, the operator signs in, and pastes the resulting URL back.
// For the CN round the same paste box carries the phone number and then the
// code, which is why CompleteLogin accepts all three shapes.
//
// Every request carries source_id="autoclaw" and a device_id, and the upstream
// keys the refresh token to that device: it must stay the same across login and
// every later refresh, so it is derived from the account id instead of being
// random.
const (
	pathSmsSendCode = "/userapi/v1/agent-send-code"
	// pathSmsLogin has NO trailing slash. The published shape of this endpoint
	// used to carry one, and an older build of the client still asks for it with
	// the slash - the gateway now answers that with a 307 to the slashless path.
	// Verified live on the deployed host: "/agent-login/" -> 307 -> "/agent-login"
	// which then answers with a business code. This client does not follow
	// redirects, so asking for the canonical path is what makes the round work.
	pathSmsLogin      = "/userapi/v1/agent-login"
	pathOAuthURL      = "/userapi/overseasv1/%s-oauth-url"
	pathOAuthLogin    = "/userapi/overseasv1/%s-oauth-login"
	pathCaptchaConfig = "/userapi/overseasv1/oauth-captcha-config"

	loginSourceID = "autoclaw"

	// loginAttemptTTL bounds how long a pasted answer is still paired with the
	// round that asked for it.
	loginAttemptTTL = 15 * time.Minute

	loginProviderZai    = "zai"
	loginProviderGoogle = "google"

	// oauthCallbackPort is the first port the desktop client tries and the
	// first one in the OAuth client's redirect whitelist. Fixed on purpose:
	// the URL below is part of what the gateway validates.
	oauthCallbackPort = 18432
)

// oauthTrustedHosts is the set of hosts the authorize URL may land on. It is
// spelled out from the client's own check because a URL pointing anywhere else
// means the link was tampered with, and handing the code over would hand over
// the account.
var oauthTrustedHosts = map[string]string{
	loginProviderZai:    "chatglm.cn",
	loginProviderGoogle: "accounts.google.com",
}

// loginAttempt is the state of one interactive round, kept in memory: it is
// worth nothing after a restart, and the operator simply starts again.
type loginAttempt struct {
	Mode        string // "sms" or "oauth"
	Phone       string
	Provider    string
	NavigateURI string
	At          time.Time
}

// loginFlow implements providers.LoginSessionProvider and LoginCompleter for
// one region. It is stateless; the round state lives on the client.
type loginFlow struct{ client *Client }

// Adapter-facing notifications.

// rememberLogin starts (or restarts) the pending round for an account.
func (c *Client) rememberLogin(accountID string, attempt loginAttempt) {
	attempt.At = c.now()
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = map[string]loginAttempt{}
	}
	c.logins[accountID] = attempt
}

// loginAttempt returns the pending round, dropping it if it has aged out.
func (c *Client) loginAttempt(accountID string) (loginAttempt, bool) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	attempt, ok := c.logins[accountID]
	if !ok {
		return loginAttempt{}, false
	}
	if c.now().Sub(attempt.At) > loginAttemptTTL {
		delete(c.logins, accountID)
		return loginAttempt{}, false
	}
	return attempt, true
}

func (c *Client) forgetLogin(accountID string) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	delete(c.logins, accountID)
}

// loginDeviceID derives a stable UUID-shaped device id for an account.
//
// The upstream keys refresh tokens to the device that logged in, so this has to
// survive restarts - and deriving it from the account id keeps it stable
// without a second place to persist state, while still being unique per
// account and per build (the two regions are separate account planes).
func (c *Client) loginDeviceID(accountID string) string {
	sum := sha256.Sum256([]byte("autoclaw-device:" + c.region.String() + ":" + accountID))
	b := sum[0:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// StartLogin opens a round. The CN round cannot send its SMS yet - the phone
// number is the operator's next answer, not a parameter of this interface - so
// it returns a session and waits; the Intl round returns the authorize URL.
func (f loginFlow) StartLogin(ctx context.Context, accountID string) (providers.LoginSession, error) {
	c := f.client
	if _, err := c.store.Get(ctx, accountID); err != nil {
		return providers.LoginSession{}, err
	}
	if c.region != RegionIntl {
		c.rememberLogin(accountID, loginAttempt{Mode: "sms"})
		return providers.LoginSession{State: "sms"}, nil
	}
	provider := loginProviderZai
	input := providers.LoginInputFromContext(ctx)
	navigateURI := fmt.Sprintf("http://localhost:%d/auth/callback-%s", oauthCallbackPort, provider)
	authURL, err := c.oauthAuthorizeURL(ctx, accountID, provider, navigateURI, input.Captcha)
	if err != nil {
		return providers.LoginSession{}, err
	}
	c.rememberLogin(accountID, loginAttempt{Mode: "oauth", Provider: provider, NavigateURI: navigateURI})
	return providers.LoginSession{AuthURL: authURL, State: "oauth"}, nil
}

// PollLogin reports what the operator should do next. It never completes a
// round by itself: both rounds end with something the operator has to paste.
func (f loginFlow) PollLogin(ctx context.Context, accountID string) (bool, string, error) {
	if _, err := f.client.store.Get(ctx, accountID); err != nil {
		return false, "", err
	}
	attempt, ok := f.client.loginAttempt(accountID)
	if !ok {
		return false, "", nil
	}
	switch {
	case attempt.Mode == "sms" && attempt.Phone == "":
		return false, "国内版走手机验证码：把 11 位手机号粘贴到下面的框，点“完成登录”后会给你发验证码", nil
	case attempt.Mode == "sms":
		return false, fmt.Sprintf("验证码已发到 %s：把收到的 6 位数字粘贴到下面的框再点“完成登录”", maskPhone(attempt.Phone)), nil
	default:
		return false, fmt.Sprintf(
			"在浏览器里打开上面的授权链接登录；完成后把地址栏那一整条（localhost:%d/auth/callback-%s?code=…&state=…）粘贴到下面的框",
			oauthCallbackPort, attempt.Provider), nil
	}
}

// CompleteLogin takes the operator's paste: a phone number (CN, sends the
// code), the SMS code (CN, finishes), or the callback URL (Intl, exchanges).
//
// Which one it is cannot be guessed from the shape alone in every case - a
// six-digit code and a phone number are both just digits - so the pending round
// decides: once it knows the phone, digits mean the code; before that, they
// mean the phone.
func (f loginFlow) CompleteLogin(ctx context.Context, accountID, pasted string) error {
	c := f.client
	if _, err := c.store.Get(ctx, accountID); err != nil {
		return err
	}
	value := strings.TrimSpace(pasted)
	if value == "" {
		return &providers.Error{Kind: accounts.KindAuth, Message: "paste the phone number, the SMS code or the callback URL first"}
	}
	attempt, ok := c.loginAttempt(accountID)
	if !ok {
		// No live round (a restart, or an operator who pasted first): rebuild
		// what the value alone can tell us.
		switch {
		case c.region != RegionIntl:
			attempt = loginAttempt{Mode: "sms"}
		case strings.Contains(value, "code="):
			provider := providerFromCallback(value)
			if provider == "" {
				return &providers.Error{Kind: accounts.KindAuth, Message: "callback URL does not name a supported provider (zai or google)"}
			}
			attempt = loginAttempt{
				Mode:        "oauth",
				Provider:    provider,
				NavigateURI: fmt.Sprintf("http://localhost:%d/auth/callback-%s", oauthCallbackPort, provider),
			}
		default:
			return &providers.Error{Kind: accounts.KindAuth, Message: "no login round is open for this account: start the login first"}
		}
	}
	if attempt.Mode == "sms" {
		return c.completeSMSLogin(ctx, accountID, attempt, value)
	}
	return c.completeOAuthLogin(ctx, accountID, attempt, value)
}

// completeSMSLogin drives the two-step phone round.
func (c *Client) completeSMSLogin(ctx context.Context, accountID string, attempt loginAttempt, value string) error {
	if attempt.Phone == "" {
		phone := normalizePhone(value)
		if phone == "" {
			return &providers.Error{Kind: accounts.KindAuth, Message: "that is not a phone number: paste the 11-digit number on the account"}
		}
		if err := c.sendSMSCode(ctx, accountID, phone); err != nil {
			return err
		}
		attempt.Phone = phone
		c.rememberLogin(accountID, attempt)
		return nil
	}
	code := smsCodeValue(value)
	if code == "" {
		return &providers.Error{Kind: accounts.KindAuth, Message: "that is not a 6-digit SMS code"}
	}
	return c.smsLogin(ctx, accountID, attempt.Phone, code)
}

// completeOAuthLogin finishes the Intl round with the pasted callback URL.
func (c *Client) completeOAuthLogin(ctx context.Context, accountID string, attempt loginAttempt, value string) error {
	code, state, err := parseOAuthCallback(value, attempt.Provider)
	if err != nil {
		return err
	}
	if strings.TrimSpace(attempt.NavigateURI) == "" {
		attempt.NavigateURI = fmt.Sprintf("http://localhost:%d/auth/callback-%s", oauthCallbackPort, attempt.Provider)
	}
	return c.oauthLogin(ctx, accountID, attempt.Provider, code, state, attempt.NavigateURI)
}

// --- upstream round trips ---------------------------------------------------

// accountEnvelope is the account plane's response shape: HTTP is 200 even for
// failures, the business result is in code.
type accountEnvelope struct {
	Code *float64        `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// loginBusinessCodes turns the account plane's business codes into something an
// operator can act on. The upstream's own msg for these is often just
// "请求数据有问题", which says nothing about what to do.
var loginBusinessCodes = map[int64]string{
	400001: "the upstream rejected the request shape (missing or malformed field)",
	400002: "the request signature was rejected",
	429:    "too many attempts, wait a moment",
	630006: "the upstream does not accept that phone number",
	630202: "the SMS code is wrong",
	630203: "too many attempts, wait a moment",
	630012: "this account was recently deleted",
	630014: "the security check failed, solve it again",
	631000: "the upstream rejected the login",
	631002: "the upstream wants an Aliyun verification proof (captcha) before it hands out an authorize URL",
	631010: "not a member of that organization, sign in with a personal account",
	631011: "the organization is disabled",
	631012: "the organization's service is unavailable, try later",
	631013: "the organization does not support this operation, use a personal account",
	631014: "this region is not supported, change the network path",
	631015: "the account belongs to an organization and has to pick an identity first",
	631016: "the identity credential expired, sign in again",
}

// accountPost posts to the account plane with the client fingerprint every
// account endpoint expects.
//
// The three X-Auth-* headers are only the middleware signature; without the
// client identity headers the signed request still gets answered with a
// business error, which is why both sets are always sent together.
func (c *Client) accountPost(ctx context.Context, accountID, path string, payload map[string]any) (json.RawMessage, error) {
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	for key, value := range payload {
		body[key] = value
	}
	body["source_id"] = loginSourceID
	body["device_id"] = c.loginDeviceID(accountID)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("autoclaw login: encode request: %w", err)
	}
	endpoint := strings.TrimRight(c.userapiBase, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("autoclaw login: build request: %w", err)
	}
	for key, value := range signedHeaders("", c.now()) {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &providers.Error{Kind: accounts.KindUnavailable, Message: "autoclaw login: " + err.Error()}
	}
	// One redirect hop is followed by hand: this client deliberately does not
	// follow redirects (the chat path uses that to see 302s as answers), but the
	// account plane moves endpoints between builds and answers 307 with the new
	// path. Retrying once keeps a moved endpoint from looking like a failed
	// login, and a loop cannot happen because only one hop is taken.
	if (resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusPermanentRedirect) &&
		resp.Header.Get("Location") != "" {
		_ = resp.Body.Close()
		target := resp.Header.Get("Location")
		if strings.HasPrefix(target, "/") {
			target = strings.TrimRight(c.userapiBase, "/") + target
		}
		moved, moveErr := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
		if moveErr == nil {
			for key, value := range signedHeaders("", c.now()) {
				moved.Header.Set(key, value)
			}
			if next, nextErr := client.Do(moved); nextErr == nil {
				resp = next
			}
		}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))

	var envelope accountEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("autoclaw login: non-JSON answer (http=%d): %.120s", resp.StatusCode, strings.TrimSpace(string(data))),
		}
	}
	if envelope.Code == nil || *envelope.Code != 0 {
		code := int64(0)
		hasCode := envelope.Code != nil
		if hasCode {
			code = int64(*envelope.Code)
		}
		reason := loginBusinessCodes[code]
		if reason == "" {
			reason = strings.TrimSpace(envelope.Msg)
		}
		if reason == "" {
			reason = fmt.Sprintf("the account plane refused the request (http=%d)", resp.StatusCode)
		} else if hasCode && code != 0 {
			reason = fmt.Sprintf("%s (code=%d)", reason, code)
		}
		kind := accounts.KindAuth
		if code == 631002 || code == 630014 || code == 429 || code == 630203 {
			kind = accounts.KindUnavailable
		}
		status := resp.StatusCode
		if status < 400 {
			status = http.StatusBadGateway
		}
		return nil, &providers.Error{Kind: kind, Status: status, Message: "autoclaw login: " + reason}
	}
	return envelope.Data, nil
}

// sendSMSCode asks the domestic build to text a login code.
func (c *Client) sendSMSCode(ctx context.Context, accountID, phone string) error {
	_, err := c.accountPost(ctx, accountID, pathSmsSendCode, map[string]any{"phone": phone})
	return err
}

// smsLogin exchanges the code for tokens and stores the credential.
func (c *Client) smsLogin(ctx context.Context, accountID, phone, code string) error {
	data, err := c.accountPost(ctx, accountID, pathSmsLogin, map[string]any{
		"phone": phone,
		// The code goes up as a JSON number: the upstream type-checks it and
		// answers 400001 for a string.
		"code": smsCodeNumber(code),
	})
	if err != nil {
		return err
	}
	_, err = c.storeLoginCredential(ctx, accountID, data, phone)
	return err
}

// captchaConfig is the Aliyun widget configuration the gateway publishes for
// the authorize step. Reported to the operator when a proof is required: the
// scene parameters are what a browser has to render to solve it.
type captchaConfig struct {
	Enabled  bool   `json:"enabled"`
	Region   string `json:"region"`
	Prefix   string `json:"prefix"`
	SceneID  string `json:"scene_id"`
	Supplier string `json:"captcha_supplier"`
}

func (c *Client) fetchCaptchaConfig(ctx context.Context, accountID string) (captchaConfig, error) {
	data, err := c.accountPost(ctx, accountID, pathCaptchaConfig, map[string]any{})
	if err != nil {
		return captchaConfig{}, err
	}
	var config captchaConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return captchaConfig{}, &providers.Error{Kind: accounts.KindUnavailable, Message: "autoclaw login: captcha config is not JSON"}
	}
	return config, nil
}

// explainCaptcha turns "the authorize step was refused" into something an
// operator can act on.
//
// Verified against the live gateway: /overseasv1/zai-oauth-url answers
// code 631002 ("当前版本已停止服务") for a request without a proof, and
// /overseasv1/oauth-captcha-config publishes the Aliyun scene that would have
// to be solved. There is no honest way to mint that proof from here (an
// automated solver is scored as a bot), so the message names the two paths that
// do work: solve it in a browser and pass the proof, or use the domestic
// phone-code login, which no captcha guards.
func (c *Client) explainCaptcha(ctx context.Context, accountID string, cause error) error {
	if cause == nil || !strings.Contains(cause.Error(), "631002") {
		return cause
	}
	config, err := c.fetchCaptchaConfig(ctx, accountID)
	if err != nil || !config.Enabled {
		return cause
	}
	return &providers.Error{
		Kind: accounts.KindUnavailable,
		Message: fmt.Sprintf(
			"autoclaw login: 国际版授权链接需要先过 Aliyun 安全验证（scene_id=%s prefix=%s region=%s supplier=%s），"+
				"这一步在浏览器里完成；把拿到的验证参数填进 captcha 后重试。"+
				"国内账号不需要它：国内版用手机验证码登录即可",
			config.SceneID, config.Prefix, config.Region, config.Supplier),
	}
}

// oauthAuthorizeURL asks for the authorize link the operator opens in a
// browser.
func (c *Client) oauthAuthorizeURL(ctx context.Context, accountID, provider, navigateURI, captcha string) (string, error) {
	payload := map[string]any{"navigate_uri": navigateURI}
	if value := strings.TrimSpace(captcha); value != "" {
		payload["ali_captcha_verify_param"] = value
	}
	data, err := c.accountPost(ctx, accountID, fmt.Sprintf(pathOAuthURL, provider), payload)
	if err != nil {
		return "", c.explainCaptcha(ctx, accountID, err)
	}
	var decoded struct {
		OAuthURL string `json:"oauth_url"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return "", &providers.Error{Kind: accounts.KindUnavailable, Message: "autoclaw login: authorize response is not JSON"}
	}
	link := strings.TrimSpace(decoded.OAuthURL)
	if link == "" {
		return "", &providers.Error{Kind: accounts.KindUnavailable, Message: "autoclaw login: authorize response carried no oauth_url"}
	}
	if !trustedOAuthURL(link, provider) {
		return "", &providers.Error{
			Kind:    accounts.KindUnavailable,
			Message: fmt.Sprintf("autoclaw login: authorize URL is not on %s, refusing to use it: %.120s", oauthTrustedHosts[provider], link),
		}
	}
	return link, nil
}

// oauthLogin exchanges the callback's code for tokens and stores the credential.
func (c *Client) oauthLogin(ctx context.Context, accountID, provider, code, state, navigateURI string) error {
	data, err := c.accountPost(ctx, accountID, fmt.Sprintf(pathOAuthLogin, provider), map[string]any{
		"code": code, "state": state, "navigate_uri": navigateURI,
	})
	if err != nil {
		return err
	}
	_, err = c.storeLoginCredential(ctx, accountID, data, "")
	return err
}

// loginTokens is the token payload both rounds answer with. Field names are
// snake_case here (access_token / refresh_token / user_id) - this is the
// boundary, everything downstream uses the canonical Credential.
type loginTokens struct {
	AccessToken      string `json:"access_token"`
	AccessTokenCamel string `json:"accessToken"`
	RefreshToken     string `json:"refresh_token"`
	RefreshCamel     string `json:"refreshToken"`
	UserID           string `json:"user_id"`
	UserIDCamel      string `json:"userId"`
	Phone            string `json:"user_phone"`
}

// storeLoginCredential persists what a login round produced.
//
// The expiry is read from the new token's own exp claim, and the account is
// marked ready immediately: leaving the status alone would make the console
// show a fresh login as "unknown" until the next maintenance pass.
func (c *Client) storeLoginCredential(ctx context.Context, accountID string, data json.RawMessage, phone string) (Credential, error) {
	var tokens loginTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return Credential{}, &providers.Error{Kind: accounts.KindUnavailable, Message: "autoclaw login: token response is not JSON"}
	}
	access := trimBearer(firstNonEmpty(tokens.AccessToken, tokens.AccessTokenCamel))
	refresh := trimBearer(firstNonEmpty(tokens.RefreshToken, tokens.RefreshCamel))
	userID := firstNonEmpty(tokens.UserID, tokens.UserIDCamel)
	if access == "" || refresh == "" {
		return Credential{}, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Message: "autoclaw login: the answer carried no access_token / refresh_token pair",
		}
	}
	cred := Credential{
		Format:       c.region.CredentialFormat(),
		AccessToken:  access,
		RefreshToken: refresh,
		DeviceID:     c.loginDeviceID(accountID),
		Region:       c.region.String(),
		UserID:       firstNonEmpty(userID, jwtUserID(access)),
	}
	if expiry, ok := jwtExpiry(access); ok {
		cred.ExpiresAt = expiry
	}
	if err := c.saveCredential(ctx, accountID, cred); err != nil {
		return Credential{}, err
	}
	_ = c.store.Observe(ctx, accountID, cred.UserID, "ready", "", "")
	c.forgetLogin(accountID)
	return cred, nil
}

// --- input helpers ----------------------------------------------------------

var smsCodePattern = regexp.MustCompile(`(?:^|\D)(\d{6})(?:\D|$)`)

// normalizePhone accepts the ways an operator might write a CN mobile number:
// with or without +86, with spaces or dashes, or as the raw 11 digits.
func normalizePhone(raw string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.TrimSpace(raw))
	if len(digits) == 13 && strings.HasPrefix(digits, "86") {
		digits = digits[2:]
	}
	if len(digits) != 11 || digits[0] != '1' {
		return ""
	}
	return digits
}

// smsCodeValue extracts the six digits of a code from whatever the operator
// pasted (just the digits, or a sentence around them).
func smsCodeValue(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) == 6 && smsCodeNumber(trimmed) != 0 {
		return trimmed
	}
	if match := smsCodePattern.FindStringSubmatch(trimmed); match != nil {
		return match[1]
	}
	return ""
}

// smsCodeNumber converts a code to the number the upstream expects.
func smsCodeNumber(code string) int64 {
	var value int64
	for _, r := range code {
		if r < '0' || r > '9' {
			return 0
		}
		value = value*10 + int64(r-'0')
	}
	return value
}

// providerFromCallback reads the provider out of a callback URL's path.
func providerFromCallback(raw string) string {
	for provider := range oauthTrustedHosts {
		if strings.Contains(raw, "/auth/callback-"+provider) {
			return provider
		}
	}
	return ""
}

// parseOAuthCallback pulls code and state out of the pasted callback.
//
// Operators paste three things: the whole URL, a bare query string, or the
// fragment they could select. All three are accepted, but when a URL is given
// its path has to be the callback path for that provider - a paste from some
// other round would otherwise be exchanged against this one.
func parseOAuthCallback(raw, provider string) (string, string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "", &providers.Error{Kind: accounts.KindAuth, Message: "autoclaw login: nothing was pasted"}
	}
	query := ""
	if strings.Contains(value, "?") {
		parts := strings.SplitN(value, "?", 2)
		if !strings.Contains(parts[0], "/auth/callback-") {
			return "", "", &providers.Error{
				Kind:    accounts.KindAuth,
				Message: "autoclaw login: pasted URL is not the login callback (expected /auth/callback-" + provider + ")",
			}
		}
		if provider != "" && !strings.Contains(parts[0], "/auth/callback-"+provider) {
			return "", "", &providers.Error{
				Kind:    accounts.KindAuth,
				Message: "autoclaw login: pasted URL belongs to a different provider than the open round",
			}
		}
		query = parts[1]
	} else {
		query = value
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", "", &providers.Error{Kind: accounts.KindAuth, Message: "autoclaw login: pasted callback is not a query string"}
	}
	code := strings.TrimSpace(values.Get("code"))
	state := strings.TrimSpace(values.Get("state"))
	if code == "" || state == "" {
		return "", "", &providers.Error{
			Kind:    accounts.KindAuth,
			Message: "autoclaw login: the callback has no code/state - the sign-in was probably cancelled",
		}
	}
	return code, state, nil
}

// trustedOAuthURL reports whether an authorize link lands on the host the
// provider is supposed to use: https, no credentials, no explicit port.
func trustedOAuthURL(raw, provider string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" {
		return false
	}
	expected := oauthTrustedHosts[provider]
	if expected == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, expected)
}

// maskPhone keeps the last four digits: the operator needs to recognize the
// number, and a log or a screenshot should not carry the whole thing.
func maskPhone(phone string) string {
	if len(phone) < 4 {
		return phone
	}
	return strings.Repeat("*", len(phone)-4) + phone[len(phone)-4:]
}
