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

// Browser login. The ZCode desktop client signs in to two independent OAuth
// realms and each one is its own round trip, so the flow below is always
// driven from a per-region profile rather than from shared constants:
//
//   - zai (chat.z.ai) — the client's default service. Authorize on
//     chat.z.ai/api/oauth/authorize, and an extra business exchange
//     (api.z.ai/api/auth/z/login) that mints the ZCode JWT from the provider
//     access token.
//   - bigmodel (bigmodel.cn) — authorize on bigmodel.cn/login; its token
//     response already carries the ZCode JWT.
//
// Z.ai signs in through the round trip the ZCode CLI runs: the plan gateway
// hands back an authorize URL whose redirect is its own callback
// (zcode.z.ai/api/v1/oauth/cli/callback/zai), and the operator never has to
// hand a code back. The code cannot be swallowed on the way either — whoever
// registered the zcode:// scheme on that machine (an account manager such as
// cockpit-tools, say) is not part of this flow. The console polls instead:
// StartLogin mints the flow upstream, PollLogin asks until it answers ready, and
// the credential is stored right there.
//
// The paste round trip stays as the fallback for Z.ai and remains the only flow
// BigModel has: both realms register a zcode:// custom scheme, the pasted link
// must carry the state this process minted, and the redirect the browser
// actually used is echoed back to the token endpoint, which rejects a mismatch.
const (
	loginTokenURL = "https://zcode.z.ai/api/v1/oauth/token"

	// Z.ai realm (region zai).
	zaiAuthorizeURL = "https://chat.z.ai/api/oauth/authorize"
	zaiClientID     = "client_P8X5CMWmlaRO9gyO-KSqtg"
	// zaiRedirectURI is the callback Z.ai registers for its desktop client; the
	// paste round trip asks for it.
	zaiRedirectURI       = "zcode://zai-auth/callback"
	zaiLegacyRedirectURI = "zcode://oauth/callback"
	zaiUserInfoURL       = "https://chat.z.ai/api/oauth/userinfo"
	zaiBusinessLoginURL  = "https://api.z.ai/api/auth/z/login"

	// The Z.ai CLI sign-in lives on the plan gateway and redirects back to it,
	// so neither a custom scheme nor a local port is involved.
	zaiCLIBaseURL  = "https://zcode.z.ai/api/v1"
	zaiCLIInitPath = "/oauth/cli/init"
	zaiCLIPollPath = "/oauth/cli/poll/"
	zaiCLIProvider = "zai"

	// BigModel realm (region bigmodel).
	bigmodelAuthorizeURL    = "https://bigmodel.cn/login"
	bigmodelAppID           = "zcode"
	bigmodelRedirectURI     = "zcode://oauth/callback"
	bigmodelLegacyRedirect  = "zcode://bigmodel-auth/callback"
	bigmodelUserInfoURL     = "https://zcode.z.ai/api/oauth/userinfo"
	bigmodelCustomerInfoURL = "https://open.bigmodel.cn/api/biz/customer/getCustomerInfo"

	// loginPendingTTL bounds how long a pasted callback is still paired with
	// the state it was minted for.
	loginPendingTTL = 15 * time.Minute
	// loginExchangeTimeout bounds the token exchange so a stuck upstream
	// cannot hold the console request open.
	loginExchangeTimeout = 30 * time.Second
)

// loginRealm is the OAuth profile of one region: authorize host, redirect
// schemes, token-exchange dialect and identity endpoint. The two services share
// only the token URL and the credential shape.
type loginRealm struct {
	region       string
	authorizeURL string
	// authorizeClientID is the client_id / appId parameter the authorize host
	// expects.
	authorizeClientID string
	// bigmodelDialect distinguishes the two authorize dialects: Z.ai uses
	// client_id/response_type/redirect_uri, BigModel uses redirect/appId.
	bigmodelDialect bool
	// authorizeRedirectURI is the redirect the authorize URL asks for. It has
	// to be registered against the realm's client id.
	authorizeRedirectURI string
	// registeredRedirectURIs are every redirect registered for that client id
	// and therefore accepted when the operator pastes the callback back. The
	// token exchange echoes the one the browser actually used.
	registeredRedirectURIs []string
	// tokenProvider is the "provider" value the token endpoint expects.
	tokenProvider string
	userInfoURL   string
	// businessLoginURL, when set, mints the ZCode JWT from the provider access
	// token (Z.ai). Empty means the token response already carries the JWT.
	businessLoginURL string
	// customerInfoURL is a region-specific identity fallback, tried only when
	// the shared userinfo endpoint yields nothing.
	customerInfoURL string
}

// loginRealmFor maps an account region onto its profile. An empty or unknown
// region falls back to the descriptor default so an account row created before
// the region existed still logs in.
func loginRealmFor(region string) loginRealm {
	switch normalizeRegion(region) {
	case RegionBigModel:
		return loginRealm{
			region:               RegionBigModel,
			authorizeURL:         bigmodelAuthorizeURL,
			authorizeClientID:    bigmodelAppID,
			bigmodelDialect:      true,
			authorizeRedirectURI: bigmodelRedirectURI,
			registeredRedirectURIs: []string{
				bigmodelRedirectURI,
				bigmodelLegacyRedirect,
			},
			tokenProvider:   RegionBigModel,
			userInfoURL:     bigmodelUserInfoURL,
			customerInfoURL: bigmodelCustomerInfoURL,
		}
	default:
		return loginRealm{
			region:               RegionZAI,
			authorizeURL:         zaiAuthorizeURL,
			authorizeClientID:    zaiClientID,
			authorizeRedirectURI: zaiRedirectURI,
			registeredRedirectURIs: []string{
				zaiRedirectURI,
				zaiLegacyRedirectURI,
			},
			tokenProvider:    RegionZAI,
			userInfoURL:      zaiUserInfoURL,
			businessLoginURL: zaiBusinessLoginURL,
		}
	}
}

// authorize builds the URL the console opens in a browser tab. The parameter
// names are the ones each service validates; renaming them breaks the round
// trip.
func (r loginRealm) authorize(state string) string {
	query := url.Values{}
	if r.bigmodelDialect {
		query.Set("redirect", r.authorizeRedirectURI)
		query.Set("appId", r.authorizeClientID)
	} else {
		query.Set("client_id", r.authorizeClientID)
		query.Set("redirect_uri", r.authorizeRedirectURI)
		query.Set("response_type", "code")
	}
	query.Set("state", state)
	return r.authorizeURL + "?" + query.Encode()
}

// matchRegisteredRedirect maps a pasted callback onto a redirect registered for
// this realm and returns the canonical spelling to echo to the token endpoint.
// Comparing the parts rather than the raw strings keeps a browser that spells
// the loopback slightly differently (trailing slash, default port) from failing
// the exchange on a technicality.
func (r loginRealm) matchRegisteredRedirect(parsed *url.URL) (string, bool) {
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	path := strings.TrimSuffix(strings.ToLower(parsed.Path), "/")
	for _, candidate := range r.registeredRedirectURIs {
		reference, err := url.Parse(candidate)
		if err != nil {
			continue
		}
		if strings.ToLower(reference.Scheme) != scheme ||
			strings.ToLower(reference.Hostname()) != host ||
			reference.Port() != port ||
			strings.TrimSuffix(strings.ToLower(reference.Path), "/") != path {
			continue
		}
		return candidate, true
	}
	return "", false
}

// callbackTargets lists the accepted callbacks for error messages.
func (r loginRealm) callbackTargets() string {
	return strings.Join(r.registeredRedirectURIs, " / ")
}

// loginPending is one browser-login round for an account: the state we minted,
// the realm the round runs against, and when. The callback must echo that
// state, so a stale link from an earlier round cannot complete a newer session.
// cli, when set, marks a round that runs the Z.ai CLI sign-in: upstream holds
// the state (the operator never sees a callback), so the poll below owns it.
type loginPending struct {
	state     string
	realm     loginRealm
	createdAt time.Time
	cli       *cliLoginFlow
}

// cliLoginFlow is the state of one Z.ai CLI sign-in round. flowID names the
// round upstream, pollToken authenticates the poll (it is the bearer of that
// round only), and expiresAt bounds how long the operator has to sign in.
type cliLoginFlow struct {
	flowID    string
	pollToken string
	expiresAt time.Time
}

// StartLogin opens a login round for the account's region and returns the URL
// the console opens in a browser tab.
//
// The Z.ai realm runs the CLI sign-in: the authorize URL redirects back to the
// plan gateway, which is what the poll below reads. Every other round is the
// paste fallback, where the state below is the one the pasted callback must
// echo.
func (c *Client) StartLogin(ctx context.Context, accountID string) (providers.LoginSession, error) {
	realm := loginRealmFor(c.accountRegion(ctx, accountID))
	state, err := loginState()
	if err != nil {
		return providers.LoginSession{}, err
	}
	pending := &loginPending{state: state, realm: realm, createdAt: time.Now()}
	if realm.region == RegionZAI {
		flow, authorizeURL, err := c.startCLILogin(ctx)
		if err == nil {
			pending.cli = flow
			c.setLoginPending(accountID, pending)
			return providers.LoginSession{AuthURL: authorizeURL, State: state}, nil
		}
		// The operator can still paste a callback; the poll's message says so.
	}
	c.setLoginPending(accountID, pending)
	return providers.LoginSession{AuthURL: realm.authorize(state), State: state}, nil
}

// startCLILogin mints one sign-in flow on the plan gateway. The poll token is
// ours: upstream authenticates the poll with it and nothing else, so no token
// that outlives the round is created here.
func (c *Client) startCLILogin(ctx context.Context) (*cliLoginFlow, string, error) {
	pollToken, err := loginState()
	if err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(map[string]string{"provider": zaiCLIProvider})
	if err != nil {
		return nil, "", err
	}
	payload, err := c.requestJSON(ctx, http.MethodPost, c.cliEndpoint(zaiCLIInitPath), "Bearer "+pollToken, body)
	if err != nil {
		return nil, "", fmt.Errorf("starting the Z.ai sign-in failed: %w", err)
	}
	envelope, err := decodeAPIEnvelope(payload, "sign-in start")
	if err != nil {
		return nil, "", err
	}
	if err := envelope.rejection("sign-in start"); err != nil {
		return nil, "", err
	}
	flow := &cliLoginFlow{
		flowID:    envelope.str("flow_id"),
		pollToken: firstNonEmptyString(envelope.str("poll_token"), pollToken),
		expiresAt: time.Now().Add(loginPendingTTL),
	}
	if seconds := envelope.int("expires_at"); seconds > 0 {
		flow.expiresAt = time.Unix(seconds, 0)
	}
	if flow.flowID == "" || envelope.str("authorize_url") == "" {
		return nil, "", fmt.Errorf("starting the Z.ai sign-in failed: the gateway returned no flow id or authorize URL")
	}
	return flow, envelope.str("authorize_url"), nil
}

// pollCLILogin asks the gateway whether the operator finished the CLI sign-in.
// A pending round only moves the console's message along; ready stores the
// credential and ends the round, which is why the poll owns the save.
func (c *Client) pollCLILogin(ctx context.Context, accountID string, pending *loginPending) (bool, string, error) {
	if time.Now().After(pending.cli.expiresAt) {
		c.clearLoginPending(accountID)
		return false, "", fmt.Errorf("login expired; start again")
	}
	payload, err := c.requestJSON(ctx, http.MethodGet, c.cliEndpoint(zaiCLIPollPath+url.PathEscape(pending.cli.flowID)), "Bearer "+pending.cli.pollToken, nil)
	if err != nil {
		return false, "", fmt.Errorf("polling the Z.ai sign-in failed: %w", err)
	}
	envelope, err := decodeAPIEnvelope(payload, "sign-in poll")
	if err != nil {
		return false, "", err
	}
	if err := envelope.rejection("sign-in poll"); err != nil {
		return false, "", err
	}
	switch status := strings.ToLower(envelope.str("status")); status {
	case "pending":
		return false, fmt.Sprintf("授权页已打开：在页面里完成登录就行，这个窗口会自己变成「已完成」，不用粘贴任何链接（登录有效期到 %s）", pending.cli.expiresAt.Local().Format("15:04")), nil
	case "failed":
		c.clearLoginPending(accountID)
		return false, "", fmt.Errorf("authorization was rejected in the browser; start the login again")
	case "ready":
		credential, err := c.cliCredential(ctx, pending.realm, envelope)
		if err != nil {
			return false, "", err
		}
		if err := c.storeCredential(ctx, accountID, credential); err != nil {
			return false, "", err
		}
		return true, "登录完成", nil
	default:
		return false, "", fmt.Errorf("sign-in poll returned an unexpected status %q", status)
	}
}

// cliCredential turns a ready poll into the credential to store. The gateway
// answers with everything the paste exchange had to assemble in three calls:
// the ZCode JWT, the provider access token and the account identity.
func (c *Client) cliCredential(ctx context.Context, realm loginRealm, envelope apiEnvelope) (Credential, error) {
	jwt := envelope.str("token")
	if !IsJWTToken(jwt) {
		return Credential{}, fmt.Errorf("sign-in poll returned no usable ZCode JWT")
	}
	credential := Credential{
		Format:      CredentialFormat,
		AuthMode:    AuthModeOAuth,
		Provider:    realm.region,
		ZCodeJWT:    jwt,
		AccessToken: objectString(envelope.object(RegionZAI), "access_token", "accessToken"),
	}
	if user := envelope.object("user"); user != nil {
		credential.UserID = objectString(user, "user_id", "userId", "id")
		credential.Email = objectString(user, "email")
		if credential.Email == "" {
			if name := objectString(user, "name", "username"); strings.Contains(name, "@") {
				credential.Email = name
			}
		}
	}
	fillJWTIdentity(&credential, jwt)
	if credential.Email == "" || credential.UserID == "" {
		email, userID := c.realmIdentity(ctx, realm, credential.AccessToken)
		if credential.Email == "" {
			credential.Email = email
		}
		if credential.UserID == "" {
			credential.UserID = userID
		}
	}
	return credential, nil
}

// setLoginPending records the round for an account, replacing any earlier one.
func (c *Client) setLoginPending(accountID string, pending *loginPending) {
	c.mu.Lock()
	if c.pending == nil {
		c.pending = map[string]*loginPending{}
	}
	c.pending[accountID] = pending
	c.mu.Unlock()
}

// loginPendingFor reads the account's round, if any.
func (c *Client) loginPendingFor(accountID string) *loginPending {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending[accountID]
}

// accountRegion reads the region the account was created for. A store failure
// falls back to the descriptor default: the authorize URL is still valid, the
// console just gets the default service.
func (c *Client) accountRegion(ctx context.Context, accountID string) string {
	if c.store == nil {
		return providers.ZCode.DefaultRegion
	}
	account, err := c.store.Get(ctx, accountID)
	if err != nil {
		return providers.ZCode.DefaultRegion
	}
	if region := strings.TrimSpace(account.ProviderRegion); region != "" {
		return region
	}
	if provider := strings.TrimSpace(account.Provider); provider != "" {
		// An account row may carry the region in provider ("zcode/bigmodel").
		if _, after, ok := strings.Cut(provider, "/"); ok {
			return after
		}
	}
	return providers.ZCode.DefaultRegion
}

// PollLogin reports the state of the account's login round. A Z.ai round is
// upstream's own sign-in flow, so this is where it completes: the gateway
// answers ready once the operator has signed in, and the credential is stored
// here. The paste round trip has nothing to poll — only a message saying what
// to paste back.
func (c *Client) PollLogin(ctx context.Context, accountID string) (bool, string, error) {
	pending := c.loginPendingFor(accountID)
	if pending == nil {
		return false, "", fmt.Errorf("login not started for account %s", accountID)
	}
	if pending.cli != nil {
		return c.pollCLILogin(ctx, accountID, pending)
	}
	if time.Since(pending.createdAt) > loginPendingTTL {
		c.clearLoginPending(accountID)
		return false, "", fmt.Errorf("login expired; start again")
	}
	target := pending.realm.authorizeRedirectURI
	return false, fmt.Sprintf("在打开的页面完成登录后浏览器会跳到 %s?...（不会自动回到这里），把地址栏那条完整链接粘贴到下面的输入框", target), nil
}

// storeCredential writes a freshly minted credential and marks the account
// ready. The paste exchange and the CLI poll both end here.
func (c *Client) storeCredential(ctx context.Context, accountID string, credential Credential) error {
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

// CompleteLogin exchanges the pasted callback URL's authorization code for
// tokens and stores the resulting credential on the account.
func (c *Client) CompleteLogin(ctx context.Context, accountID, callbackURL string) error {
	pending := c.loginPendingFor(accountID)
	if pending == nil {
		return fmt.Errorf("login not started for account %s: use the browser login first", accountID)
	}
	if pending.cli != nil {
		return fmt.Errorf("这个账号的登录不用粘贴：在打开的授权页里完成登录，控制台会自己检测到")
	}
	if time.Since(pending.createdAt) > loginPendingTTL {
		c.clearLoginPending(accountID)
		return fmt.Errorf("login expired; start again")
	}
	code, redirectURI, err := parseCallbackURL(callbackURL, pending.state, pending.realm)
	if err != nil {
		return err
	}
	credential, err := c.exchangeAuthorizationCode(ctx, pending.realm, code, pending.state, redirectURI)
	if err != nil {
		return err
	}
	return c.storeCredential(ctx, accountID, credential)
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

// parseCallbackURL validates the pasted callback URL and returns its
// authorization code plus the registered redirect it came from, which the token
// exchange has to echo. The state must match the round the console started, so
// a link from an older login cannot be replayed here.
func parseCallbackURL(raw, expectedState string, realm loginRealm) (string, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", fmt.Errorf("paste the callback URL from the browser address bar (it starts with %s)", realm.authorizeRedirectURI)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", "", fmt.Errorf("invalid callback URL: %v", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "zcode", "http", "https":
	default:
		return "", "", fmt.Errorf("callback must be the %s URL (the address-bar link), got %q", realm.authorizeRedirectURI, clip(trimmed, 40))
	}
	redirectURI, ok := realm.matchRegisteredRedirect(parsed)
	if !ok {
		return "", "", fmt.Errorf("unexpected callback target %q, want %s", parsed.Host+parsed.Path, realm.callbackTargets())
	}
	query := parsed.Query()
	if failure := strings.TrimSpace(query.Get("error")); failure != "" {
		return "", "", fmt.Errorf("authorization was rejected: %s", failure)
	}
	state := strings.TrimSpace(query.Get("state"))
	if state == "" {
		return "", "", fmt.Errorf("callback URL carries no state")
	}
	if expectedState != "" && state != expectedState {
		return "", "", fmt.Errorf("state mismatch (the link belongs to an earlier login); start the browser login again")
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		code = strings.TrimSpace(query.Get("authCode"))
	}
	if code == "" {
		return "", "", fmt.Errorf("callback URL carries no code")
	}
	return code, redirectURI, nil
}

// apiEnvelope is the {code,msg,data} shape every ZCode endpoint answers with.
// data stays raw because the provider-specific sub-object (data.zai /
// data.bigmodel) is keyed by the realm's provider id.
type apiEnvelope struct {
	Code    *int                       `json:"code"`
	Msg     string                     `json:"msg"`
	Success *bool                      `json:"success"`
	Data    map[string]json.RawMessage `json:"data"`
}

func decodeAPIEnvelope(payload []byte, op string) (apiEnvelope, error) {
	var envelope apiEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return envelope, fmt.Errorf("%s returned an unreadable body: %v", op, err)
	}
	return envelope, nil
}

// rejection turns a non-zero upstream code into the console-visible error.
func (e apiEnvelope) rejection(op string) error {
	if e.Code != nil && *e.Code != 0 {
		return fmt.Errorf("%s rejected: %s", op, firstNonEmptyString(strings.TrimSpace(e.Msg), fmt.Sprintf("code %d", *e.Code)))
	}
	if e.Success != nil && !*e.Success {
		return fmt.Errorf("%s rejected: %s", op, firstNonEmptyString(strings.TrimSpace(e.Msg), "upstream reported failure"))
	}
	return nil
}

// str reads data.<key> as a string.
func (e apiEnvelope) str(key string) string {
	return objectString(e.Data, key)
}

// object reads data.<key> as a raw object.
func (e apiEnvelope) object(key string) map[string]json.RawMessage {
	return objectMap(e.Data, key)
}

// int reads data.<key> as an integer, tolerating a string spelling.
func (e apiEnvelope) int(key string) int64 {
	raw, ok := e.Data[key]
	if !ok {
		return 0
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		var parsed int64
		if _, err := fmt.Sscanf(strings.TrimSpace(text), "%d", &parsed); err == nil {
			return parsed
		}
	}
	return 0
}

func objectMap(fields map[string]json.RawMessage, key string) map[string]json.RawMessage {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw, &inner); err != nil {
		return nil
	}
	return inner
}

func objectString(fields map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			continue
		}
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// exchangeAuthorizationCode posts the authorization code to the ZCode token
// endpoint and turns the response into a stored credential. redirectURI is the
// registered redirect the browser actually used; the endpoint rejects the
// exchange when it does not match the one the code was issued for.
//
// The realm decides the body's provider value and where the ZCode JWT comes
// from: BigModel answers with data.token directly, while Z.ai hands back the
// provider access token and the JWT is minted by the business login exchange.
func (c *Client) exchangeAuthorizationCode(ctx context.Context, realm loginRealm, code, state, redirectURI string) (Credential, error) {
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = realm.authorizeRedirectURI
	}
	body, err := json.Marshal(map[string]string{
		"provider":     realm.tokenProvider,
		"code":         code,
		"redirect_uri": redirectURI,
		"state":        state,
	})
	if err != nil {
		return Credential{}, err
	}
	payload, err := c.postJSON(ctx, c.tokenEndpoint(), body)
	if err != nil {
		return Credential{}, fmt.Errorf("token exchange failed: %w", err)
	}
	envelope, err := decodeAPIEnvelope(payload, "token exchange")
	if err != nil {
		return Credential{}, err
	}
	if err := envelope.rejection("token exchange"); err != nil {
		return Credential{}, err
	}
	tokens := envelope.object(realm.tokenProvider)
	accessToken := firstNonEmptyString(objectString(tokens, "access_token", "accessToken"), envelope.str("access_token"))
	refreshToken := firstNonEmptyString(objectString(tokens, "refresh_token", "refreshToken"), envelope.str("refresh_token"))
	jwt := envelope.str("token")
	if realm.businessLoginURL != "" {
		minted, err := c.businessLogin(ctx, realm, accessToken)
		if err != nil {
			// Tolerated only when the token response already carried a JWT:
			// otherwise the credential would be unusable.
			if !IsJWTToken(jwt) {
				return Credential{}, err
			}
		} else {
			jwt = minted
		}
	}
	if jwt == "" {
		return Credential{}, fmt.Errorf("token exchange response carries no ZCode JWT")
	}
	credential := Credential{
		Format:       CredentialFormat,
		AuthMode:     AuthModeOAuth,
		Provider:     realm.region,
		ZCodeJWT:     jwt,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}
	if expires := envelope.int("expires_in"); expires > 0 {
		credential.ExpiresAt = time.Now().Add(time.Duration(expires) * time.Second).Unix()
	}
	if user := envelope.object("user"); user != nil {
		credential.UserID = objectString(user, "id", "user_id", "userId")
		if name := objectString(user, "username", "name", "email"); strings.Contains(name, "@") {
			credential.Email = name
		}
	}
	fillJWTIdentity(&credential, jwt)
	if accessToken != "" {
		email, userID := c.realmIdentity(ctx, realm, accessToken)
		if credential.Email == "" {
			credential.Email = email
		}
		if credential.UserID == "" {
			credential.UserID = userID
		}
	}
	return credential, nil
}

// businessLogin mints the ZCode JWT from a provider access token (the Z.ai
// realm's extra step). The response's data.access_token is the token the chat
// and plan endpoints accept.
func (c *Client) businessLogin(ctx context.Context, realm loginRealm, providerToken string) (string, error) {
	if strings.TrimSpace(providerToken) == "" {
		return "", fmt.Errorf("minting the ZCode JWT failed: the token exchange returned no provider access token")
	}
	endpoint := strings.TrimSpace(c.businessLoginURL)
	if endpoint == "" {
		endpoint = realm.businessLoginURL
	}
	body, err := json.Marshal(map[string]string{"token": providerToken})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	for key, value := range defaultHeaders() {
		request.Header.Set(key, value)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("minting the ZCode JWT failed: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("minting the ZCode JWT failed: %w", err)
	}
	if response.StatusCode >= 300 {
		return "", fmt.Errorf("minting the ZCode JWT failed: HTTP %d %s", response.StatusCode, clip(strings.TrimSpace(string(payload)), 200))
	}
	envelope, err := decodeAPIEnvelope(payload, "business login")
	if err != nil {
		return "", err
	}
	if err := envelope.rejection("business login"); err != nil {
		return "", err
	}
	token := envelope.str("access_token")
	if token == "" {
		return "", fmt.Errorf("minting the ZCode JWT failed: business login returned no access_token")
	}
	return token, nil
}

// realmIdentity reads the account identity from the realm's userinfo endpoint
// (and, for BigModel, its customer endpoint). It is best-effort: login succeeds
// on the tokens alone, and this only fills the console's email / user id
// labels.
func (c *Client) realmIdentity(ctx context.Context, realm loginRealm, accessToken string) (string, string) {
	endpoint := strings.TrimSpace(c.userInfoURL)
	if endpoint == "" {
		endpoint = realm.userInfoURL
	}
	// The shared userinfo endpoint takes a Bearer token; the BigModel customer
	// endpoint takes the raw access token.
	if email, userID := c.identityFrom(ctx, endpoint, "Bearer "+accessToken); email != "" || userID != "" {
		return email, userID
	}
	if realm.customerInfoURL == "" {
		return "", ""
	}
	customer := strings.TrimSpace(c.customerURL)
	if customer == "" {
		customer = realm.customerInfoURL
	}
	return c.identityFrom(ctx, customer, accessToken)
}

func (c *Client) identityFrom(ctx context.Context, endpoint, authorization string) (string, string) {
	if strings.TrimSpace(endpoint) == "" {
		return "", ""
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", ""
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", authorization)
	response, err := c.http.Do(request)
	if err != nil {
		return "", ""
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode >= 300 {
		return "", ""
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		return "", ""
	}
	fields := document
	if data, ok := document["data"].(map[string]any); ok && len(data) > 0 {
		fields = data
	}
	email := pickLocalField(fields, "email", "userEmail")
	if email == "" {
		if name := pickLocalField(fields, "username", "name"); strings.Contains(name, "@") {
			email = name
		}
	}
	userID := pickLocalField(fields, "user_id", "userId", "sub", "id", "customerNumber")
	return email, userID
}

// tokenEndpoint is the configurable token URL; tests point it at a local
// server.
func (c *Client) tokenEndpoint() string {
	if endpoint := strings.TrimSpace(c.tokenURL); endpoint != "" {
		return endpoint
	}
	return loginTokenURL
}

// cliEndpoint resolves a Z.ai CLI sign-in path against the gateway base; tests
// point that base at a local server.
func (c *Client) cliEndpoint(path string) string {
	base := strings.TrimSpace(c.cliBaseURL)
	if base == "" {
		base = zaiCLIBaseURL
	}
	return strings.TrimSuffix(base, "/") + path
}

// postJSON sends a JSON POST and returns the body, mapping transport and HTTP
// failures onto errors the console can show.
func (c *Client) postJSON(ctx context.Context, endpoint string, body []byte) ([]byte, error) {
	return c.requestJSON(ctx, http.MethodPost, endpoint, "", body)
}

// requestJSON sends one JSON request and returns the body. authorization, when
// set, is sent verbatim: the CLI sign-in polls with a bearer token that belongs
// to that round alone.
func (c *Client) requestJSON(ctx context.Context, method, endpoint, authorization string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, loginExchangeTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d %s", response.StatusCode, clip(strings.TrimSpace(string(payload)), 200))
	}
	return payload, nil
}

// clip shortens a value for error messages so a pasted blob cannot flood the
// console.
func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
