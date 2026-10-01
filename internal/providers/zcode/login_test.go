package zcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// deadURL refuses connections immediately. Every login test points unset
// endpoints here so no test can ever reach a real upstream.
const deadURL = "http://127.0.0.1:1/unreachable"

// loginServers carries the local stand-ins for the four endpoints a login
// round can touch. An empty field is replaced by deadURL.
type loginServers struct {
	token    string
	identity string
	business string
	customer string
}

// newLoginClient returns a client whose login endpoints all point at local
// servers (or fail closed).
func newLoginClient(t *testing.T, store Store, servers loginServers) *Client {
	t.Helper()
	local := func(endpoint string) string {
		if strings.TrimSpace(endpoint) == "" {
			return deadURL
		}
		return endpoint
	}
	client := NewClient(store)
	client.tokenURL = local(servers.token)
	client.userInfoURL = local(servers.identity)
	client.businessLoginURL = local(servers.business)
	client.customerURL = local(servers.customer)
	return client
}

func TestLoginRealmAuthorizeURLs(t *testing.T) {
	zai := loginRealmFor(RegionZAI)
	raw := zai.authorize("state-123")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("zai authorize URL does not parse: %v", err)
	}
	if parsed.Host != "chat.z.ai" || parsed.Path != "/api/oauth/authorize" {
		t.Fatalf("zai authorize target = %s%s, want chat.z.ai/api/oauth/authorize", parsed.Host, parsed.Path)
	}
	query := parsed.Query()
	if query.Get("client_id") != zaiClientID {
		t.Errorf("client_id=%q want %q", query.Get("client_id"), zaiClientID)
	}
	if query.Get("redirect_uri") != zaiRedirectURI {
		t.Errorf("redirect_uri=%q want %q", query.Get("redirect_uri"), zaiRedirectURI)
	}
	if query.Get("response_type") != "code" {
		t.Errorf("response_type=%q want code", query.Get("response_type"))
	}
	if query.Get("state") != "state-123" {
		t.Errorf("state=%q want state-123", query.Get("state"))
	}

	// The BigModel realm keeps the desktop client's other dialect: redirect and
	// appId rather than the OAuth spelling.
	bigmodel := loginRealmFor(RegionBigModel)
	parsed, err = url.Parse(bigmodel.authorize("state-456"))
	if err != nil {
		t.Fatalf("bigmodel authorize URL does not parse: %v", err)
	}
	if parsed.Host != "bigmodel.cn" || parsed.Path != "/login" {
		t.Fatalf("bigmodel authorize target = %s%s, want bigmodel.cn/login", parsed.Host, parsed.Path)
	}
	query = parsed.Query()
	if query.Get("redirect") != bigmodelRedirectURI {
		t.Errorf("redirect=%q want %q", query.Get("redirect"), bigmodelRedirectURI)
	}
	if query.Get("appId") != bigmodelAppID {
		t.Errorf("appId=%q want %q", query.Get("appId"), bigmodelAppID)
	}
	if query.Get("state") != "state-456" {
		t.Errorf("state=%q want state-456", query.Get("state"))
	}
	if query.Get("client_id") != "" {
		t.Errorf("bigmodel authorize URL must not carry client_id, got %q", query.Get("client_id"))
	}

	// An unknown or empty region falls back to the default (Z.ai) realm.
	if loginRealmFor("").region != providers.ZCode.DefaultRegion {
		t.Errorf("empty region realm=%q want the default %q", loginRealmFor("").region, providers.ZCode.DefaultRegion)
	}
	if loginRealmFor("nonsense").region != providers.ZCode.DefaultRegion {
		t.Errorf("unknown region realm=%q want the default", loginRealmFor("nonsense").region)
	}
}

func TestParseCallbackURL(t *testing.T) {
	cases := []struct {
		name    string
		region  string
		raw     string
		state   string
		want    string
		wantErr string
	}{
		{name: "zai callback", region: RegionZAI, raw: "zcode://zai-auth/callback?code=abc&state=s1", state: "s1", want: "abc"},
		{name: "zai legacy callback", region: RegionZAI, raw: "zcode://oauth/callback?code=abc&state=s1", state: "s1", want: "abc"},
		{name: "zai rejects bigmodel callback", region: RegionZAI, raw: "zcode://bigmodel-auth/callback?code=abc&state=s1", state: "s1", wantErr: "unexpected callback target"},
		{name: "bigmodel callback", region: RegionBigModel, raw: "zcode://oauth/callback?code=abc&state=s1", state: "s1", want: "abc"},
		{name: "bigmodel legacy callback", region: RegionBigModel, raw: "zcode://bigmodel-auth/callback?code=abc&state=s1", state: "s1", want: "abc"},
		{name: "bigmodel rejects zai callback", region: RegionBigModel, raw: "zcode://zai-auth/callback?code=abc&state=s1", state: "s1", wantErr: "unexpected callback target"},
		{name: "authCode spelling", region: RegionZAI, raw: "zcode://zai-auth/callback?authCode=xyz&state=s1", state: "s1", want: "xyz"},
		{name: "urlencoded code", region: RegionZAI, raw: "zcode://zai-auth/callback?code=a%2Bb%2Fc%3D&state=s1", state: "s1", want: "a+b/c="},
		{name: "browser fragment tolerated", region: RegionZAI, raw: "zcode://zai-auth/callback?code=abc&state=s1#section", state: "s1", want: "abc"},
		{name: "empty", region: RegionZAI, raw: "   ", wantErr: "paste the"},
		{name: "wrong scheme", region: RegionZAI, raw: "https://chat.z.ai/api/oauth/authorize?code=abc&state=s1", wantErr: "zcode://"},
		{name: "no state", region: RegionZAI, raw: "zcode://zai-auth/callback?code=abc", wantErr: "no state"},
		{name: "state mismatch", region: RegionZAI, raw: "zcode://zai-auth/callback?code=abc&state=other", state: "s1", wantErr: "state mismatch"},
		{name: "provider error", region: RegionZAI, raw: "zcode://zai-auth/callback?error=access_denied&state=s1", state: "s1", wantErr: "rejected"},
		{name: "no code", region: RegionZAI, raw: "zcode://zai-auth/callback?state=s1", state: "s1", wantErr: "no code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := parseCallbackURL(tc.raw, tc.state, loginRealmFor(tc.region))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got code %q", tc.wantErr, code)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want == "" {
				return
			}
			if code != tc.want {
				t.Fatalf("code=%q want %q", code, tc.want)
			}
		})
	}
}

func TestStartLoginReturnsAuthorizeSessionAndPendingState(t *testing.T) {
	client := newLoginClient(t, &memStore{region: RegionZAI}, loginServers{})
	session, err := client.StartLogin(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if session.AuthURL == "" || session.State == "" {
		t.Fatalf("session=%+v want auth URL and state", session)
	}
	if !strings.Contains(session.AuthURL, "state="+session.State) {
		t.Fatalf("auth URL %q does not carry state %q", session.AuthURL, session.State)
	}
	if !strings.Contains(session.AuthURL, "chat.z.ai") {
		t.Fatalf("auth URL %q should target the account's Z.ai realm", session.AuthURL)
	}
	done, message, err := client.PollLogin(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if done {
		t.Fatal("PollLogin reported done before any callback was pasted")
	}
	if !strings.Contains(message, zaiRedirectURI) {
		t.Fatalf("PollLogin message %q should tell the operator what to paste", message)
	}
}

// TestStartLoginFollowsAccountRegion pins the region on the account row, not on
// a compile-time constant: a BigModel account must get the bigmodel.cn round.
func TestStartLoginFollowsAccountRegion(t *testing.T) {
	for _, tc := range []struct {
		region string
		host   string
		hint   string
	}{
		{region: RegionZAI, host: "chat.z.ai", hint: zaiRedirectURI},
		{region: RegionBigModel, host: "bigmodel.cn", hint: bigmodelRedirectURI},
	} {
		t.Run(tc.region, func(t *testing.T) {
			client := newLoginClient(t, &memStore{region: tc.region}, loginServers{})
			session, err := client.StartLogin(context.Background(), "acc1")
			if err != nil {
				t.Fatalf("StartLogin: %v", err)
			}
			if !strings.Contains(session.AuthURL, tc.host) {
				t.Fatalf("auth URL %q want host %s", session.AuthURL, tc.host)
			}
			_, message, err := client.PollLogin(context.Background(), "acc1")
			if err != nil {
				t.Fatalf("PollLogin: %v", err)
			}
			if !strings.Contains(message, tc.hint) {
				t.Fatalf("PollLogin message %q should mention %s", message, tc.hint)
			}
		})
	}
}

func TestPollLoginRejectsUnknownAndExpiredSessions(t *testing.T) {
	client := newLoginClient(t, &memStore{region: RegionZAI}, loginServers{})
	if _, _, err := client.PollLogin(context.Background(), "missing"); err == nil {
		t.Fatal("PollLogin should fail for an account with no started login")
	}
	if err := client.CompleteLogin(context.Background(), "missing", "zcode://zai-auth/callback?code=a&state=s"); err == nil {
		t.Fatal("CompleteLogin should fail for an account with no started login")
	}
	if _, err := client.StartLogin(context.Background(), "acc1"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	// Age the round past its TTL.
	client.mu.Lock()
	client.pending["acc1"].createdAt = time.Now().Add(-loginPendingTTL - time.Minute)
	client.mu.Unlock()
	if _, _, err := client.PollLogin(context.Background(), "acc1"); err == nil {
		t.Fatal("PollLogin should fail once the round expired")
	}
}

// TestCompleteLoginZaiMintsJWTThroughBusinessLogin covers the Z.ai realm's two
// steps: the token endpoint returns the provider access token, and the business
// login exchange mints the ZCode JWT the chat path uses.
func TestCompleteLoginZaiMintsJWTThroughBusinessLogin(t *testing.T) {
	minted := jwtForTest(t, map[string]any{"user_id": "u-9", "provider": RegionZAI})
	var tokenBody map[string]string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("token endpoint method=%s want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &tokenBody); err != nil {
			t.Errorf("token endpoint body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"zai":{"access_token":"zai-at","refresh_token":"zai-rt"},` +
			`"user":{"id":"u-9","username":"zai@example.com"},"expires_in":3600}}`))
	}))
	defer tokenServer.Close()

	var businessBody map[string]string
	var businessUA string
	businessServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("business login method=%s want POST", r.Method)
		}
		businessUA = r.Header.Get("User-Agent")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &businessBody); err != nil {
			t.Errorf("business login body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"success":true,"data":{"access_token":"` + minted + `","expires_in":3600}}`))
	}))
	defer businessServer.Close()

	var identityAuth string
	identityServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identityAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"sub":"u-9","name":"zai@example.com"}`))
	}))
	defer identityServer.Close()

	store := &memStore{region: RegionZAI}
	client := newLoginClient(t, store, loginServers{
		token:    tokenServer.URL,
		identity: identityServer.URL,
		business: businessServer.URL,
	})

	session, err := client.StartLogin(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	callback := zaiRedirectURI + "?code=CODE-Z&state=" + url.QueryEscape(session.State)
	if err := client.CompleteLogin(context.Background(), "acc1", callback); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	if tokenBody["provider"] != RegionZAI {
		t.Errorf("exchange provider=%q want %q", tokenBody["provider"], RegionZAI)
	}
	if tokenBody["code"] != "CODE-Z" {
		t.Errorf("exchange code=%q want CODE-Z", tokenBody["code"])
	}
	if tokenBody["redirect_uri"] != zaiRedirectURI {
		t.Errorf("exchange redirect_uri=%q want %q", tokenBody["redirect_uri"], zaiRedirectURI)
	}
	if tokenBody["state"] != session.State {
		t.Errorf("exchange state=%q want %q", tokenBody["state"], session.State)
	}
	if businessBody["token"] != "zai-at" {
		t.Errorf("business login token=%q want the provider access token zai-at", businessBody["token"])
	}
	if !strings.HasPrefix(businessUA, "ZCode/") {
		t.Errorf("business login User-Agent=%q want the ZCode client UA", businessUA)
	}
	if identityAuth != "Bearer zai-at" {
		t.Errorf("identity Authorization=%q want the Bearer provider token", identityAuth)
	}

	credential, err := DecodeCredential(store.items["acc1"])
	if err != nil {
		t.Fatalf("stored credential does not decode: %v", err)
	}
	if !credential.IsOAuth() || credential.ZCodeJWT != minted {
		t.Errorf("stored credential=%+v want the business-login minted JWT", credential)
	}
	if credential.Provider != RegionZAI {
		t.Errorf("stored provider=%q want %q", credential.Provider, RegionZAI)
	}
	if credential.AccessToken != "zai-at" || credential.RefreshToken != "zai-rt" {
		t.Errorf("stored tokens=%q/%q want zai-at/zai-rt", credential.AccessToken, credential.RefreshToken)
	}
	if credential.Email != "zai@example.com" || credential.UserID != "u-9" {
		t.Errorf("stored identity=%q/%q want zai@example.com/u-9", credential.Email, credential.UserID)
	}
	if credential.ExpiresAt <= time.Now().Unix() {
		t.Errorf("ExpiresAt=%d want a future timestamp from expires_in", credential.ExpiresAt)
	}
	// The round is consumed: replaying the same callback must fail.
	if err := client.CompleteLogin(context.Background(), "acc1", callback); err == nil {
		t.Fatal("replaying a consumed callback should fail")
	}
}

// TestCompleteLoginBigModelKeepsExchangedJWT covers the BigModel realm, whose
// token response already carries the ZCode JWT — no business login is
// involved.
func TestCompleteLoginBigModelKeepsExchangedJWT(t *testing.T) {
	jwt := jwtForTest(t, map[string]any{"user_id": "u-1", "provider": RegionBigModel})
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"token":"` + jwt + `","bigmodel":{"access_token":"at-1","refresh_token":"rt-1"},"expires_in":3600}}`))
	}))
	defer tokenServer.Close()

	identityServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"user_id":"u-1","email":"domestic@example.com"}}`))
	}))
	defer identityServer.Close()

	store := &memStore{region: RegionBigModel}
	client := newLoginClient(t, store, loginServers{token: tokenServer.URL, identity: identityServer.URL})

	session, err := client.StartLogin(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	callback := bigmodelRedirectURI + "?code=CODE-1&state=" + url.QueryEscape(session.State)
	if err := client.CompleteLogin(context.Background(), "acc1", callback); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	credential, err := DecodeCredential(store.items["acc1"])
	if err != nil {
		t.Fatalf("stored credential does not decode: %v", err)
	}
	if !credential.IsOAuth() || credential.ZCodeJWT != jwt {
		t.Errorf("stored credential=%+v want an oauth credential with the exchanged JWT", credential)
	}
	if credential.Provider != RegionBigModel {
		t.Errorf("stored provider=%q want %q", credential.Provider, RegionBigModel)
	}
	if credential.AccessToken != "at-1" || credential.RefreshToken != "rt-1" {
		t.Errorf("stored tokens=%q/%q want at-1/rt-1", credential.AccessToken, credential.RefreshToken)
	}
	if credential.Email != "domestic@example.com" || credential.UserID != "u-1" {
		t.Errorf("stored identity=%q/%q want domestic@example.com/u-1", credential.Email, credential.UserID)
	}
}

// TestBigModelIdentityFallsBackToCustomerEndpoint covers the raw-token customer
// endpoint the BigModel console uses, tried only when the shared userinfo
// endpoint answers nothing useful.
func TestBigModelIdentityFallsBackToCustomerEndpoint(t *testing.T) {
	const jwtToken = "h.p.s"
	var customerAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/userinfo":
			w.WriteHeader(http.StatusUnauthorized)
		case "/customer":
			customerAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"data":{"user_id":"u-7","email":"cust@example.com"}}`))
		default:
			t.Errorf("unexpected identity path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"token":"` + jwtToken + `","bigmodel":{"access_token":"bm-at"}}}`))
	}))
	defer tokenServer.Close()

	store := &memStore{region: RegionBigModel}
	client := newLoginClient(t, store, loginServers{
		token:    tokenServer.URL,
		identity: server.URL + "/userinfo",
		customer: server.URL + "/customer",
	})
	session, err := client.StartLogin(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := client.CompleteLogin(context.Background(), "acc1",
		bigmodelRedirectURI+"?code=c&state="+url.QueryEscape(session.State)); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if customerAuth != "bm-at" {
		t.Errorf("customer Authorization=%q want the raw access token without a Bearer prefix", customerAuth)
	}
	credential, err := DecodeCredential(store.items["acc1"])
	if err != nil {
		t.Fatalf("stored credential does not decode: %v", err)
	}
	if credential.Email != "cust@example.com" || credential.UserID != "u-7" {
		t.Errorf("stored identity=%q/%q want cust@example.com/u-7", credential.Email, credential.UserID)
	}
}

func TestCompleteLoginRejectsStateFromAnotherRound(t *testing.T) {
	tokenCalls := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		_, _ = w.Write([]byte(`{"code":0,"data":{"token":"jwt"}}`))
	}))
	defer tokenServer.Close()

	store := &memStore{region: RegionZAI}
	client := newLoginClient(t, store, loginServers{token: tokenServer.URL})
	if _, err := client.StartLogin(context.Background(), "acc1"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	err := client.CompleteLogin(context.Background(), "acc1", zaiRedirectURI+"?code=c&state=stale")
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("err=%v want a state mismatch failure", err)
	}
	if tokenCalls != 0 {
		t.Errorf("token endpoint called %d times for a mismatched state, want 0", tokenCalls)
	}
	if len(store.items) != 0 {
		t.Errorf("store=%v want nothing stored", store.items)
	}
}

func TestCompleteLoginSurfacesExchangeErrors(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3001,"msg":"parameter error"}`))
	}))
	defer tokenServer.Close()

	client := newLoginClient(t, &memStore{region: RegionZAI}, loginServers{token: tokenServer.URL})
	session, err := client.StartLogin(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	err = client.CompleteLogin(context.Background(), "acc1", zaiRedirectURI+"?code=c&state="+session.State)
	if err == nil {
		t.Fatal("CompleteLogin should fail when the token endpoint rejects the code")
	}
	if !strings.Contains(err.Error(), "3001") && !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("error %q should carry the upstream failure", err.Error())
	}
}

func TestBigModelExchangeRejectsMissingJWT(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"bigmodel":{"access_token":"at"}}}`))
	}))
	defer tokenServer.Close()

	client := newLoginClient(t, &memStore{region: RegionBigModel}, loginServers{token: tokenServer.URL})
	if _, err := client.exchangeAuthorizationCode(context.Background(), loginRealmFor(RegionBigModel), "c", "s"); err == nil {
		t.Fatal("exchange should fail when the response carries no ZCode JWT")
	}
}

func TestZaiExchangeRejectsMissingProviderToken(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"zai":{"refresh_token":"rt"}}}`))
	}))
	defer tokenServer.Close()

	client := newLoginClient(t, &memStore{region: RegionZAI}, loginServers{token: tokenServer.URL})
	_, err := client.exchangeAuthorizationCode(context.Background(), loginRealmFor(RegionZAI), "c", "s")
	if err == nil {
		t.Fatal("exchange should fail when the realm returns no provider access token to mint a JWT from")
	}
	if !strings.Contains(err.Error(), "no provider access token") {
		t.Fatalf("error %q should name the missing provider token", err.Error())
	}
}

// TestZaiExchangeFallsBackToTokenResponse covers the tolerant path: when the
// business login is unavailable but the token response already carried a JWT,
// the login still completes instead of failing the whole round.
func TestZaiExchangeFallsBackToTokenResponse(t *testing.T) {
	jwt := jwtForTest(t, map[string]any{"user_id": "u-2", "provider": RegionZAI})
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"token":"` + jwt + `","zai":{"access_token":"zai-at"}}}`))
	}))
	defer tokenServer.Close()

	client := newLoginClient(t, &memStore{region: RegionZAI}, loginServers{token: tokenServer.URL})
	credential, err := client.exchangeAuthorizationCode(context.Background(), loginRealmFor(RegionZAI), "c", "s")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if credential.ZCodeJWT != jwt || credential.AccessToken != "zai-at" || credential.Provider != RegionZAI {
		t.Errorf("credential=%+v want the token-response JWT for the zai realm", credential)
	}
}

func TestZCodeAdapterExposesBrowserLogin(t *testing.T) {
	client := NewClient(&memStore{region: RegionZAI})
	adapter := client.Adapter()
	if !adapter.Supports("login") {
		t.Fatal("zcode adapter must expose the login capability")
	}
	if _, ok := adapter.Login.(providers.LoginCompleter); !ok {
		t.Fatal("zcode login must accept a pasted callback URL (LoginCompleter)")
	}
}

func TestProviderDescriptorShipsBothRealms(t *testing.T) {
	if len(providers.ZCode.Regions) != 2 {
		t.Fatalf("zcode regions=%d want 2 (zai + bigmodel)", len(providers.ZCode.Regions))
	}
	if providers.ZCode.DefaultRegion != RegionZAI {
		t.Errorf("default region=%q want %q (the desktop client's default service)", providers.ZCode.DefaultRegion, RegionZAI)
	}
	zai, ok := providers.ZCode.Region(RegionZAI)
	if !ok {
		t.Fatal("the zai region must be advertised")
	}
	if !strings.Contains(zai.ChatBase, "api.z.ai") {
		t.Errorf("zai chat base=%q want api.z.ai", zai.ChatBase)
	}
	bigmodel, ok := providers.ZCode.Region(RegionBigModel)
	if !ok {
		t.Fatal("the bigmodel region must be advertised")
	}
	if !strings.Contains(bigmodel.ChatBase, "bigmodel.cn") {
		t.Errorf("bigmodel chat base=%q want bigmodel.cn", bigmodel.ChatBase)
	}
	if !providers.ZCode.Capabilities.BrowserLogin || !providers.ZCode.Capabilities.Login {
		t.Error("zcode descriptor must advertise the browser login")
	}
}

func TestImportAcceptsBothRealmsAndRejectsUnknownRegion(t *testing.T) {
	for _, region := range []string{RegionZAI, RegionBigModel} {
		payload := []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"` + region + `","api_key":"k1"}`)
		if err := ValidateCredential(payload); err != nil {
			t.Fatalf("ValidateCredential(%s): %v", region, err)
		}
		if _, err := (credentialCodec{}).PrepareImport(payload); err != nil {
			t.Fatalf("PrepareImport(%s): %v", region, err)
		}
	}
	unknown := []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"glm","api_key":"k1"}`)
	err := ValidateCredential(unknown)
	if err == nil {
		t.Fatal("ValidateCredential must reject an unknown zcode provider")
	}
	if !strings.Contains(err.Error(), "unknown zcode provider") {
		t.Fatalf("error %q should name the unknown provider", err.Error())
	}
	if _, err := (credentialCodec{}).PrepareImport(unknown); err == nil {
		t.Fatal("PrepareImport must reject an unknown zcode provider")
	}
}
