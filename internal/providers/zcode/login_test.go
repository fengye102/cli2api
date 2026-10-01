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

// newLoginClient returns a client whose token exchange and identity lookup
// point at local servers.
func newLoginClient(t *testing.T, store Store, tokenURL, userInfoURL string) *Client {
	t.Helper()
	client := NewClient(store)
	client.tokenURL = tokenURL
	client.userInfoURL = userInfoURL
	return client
}

func TestBuildAuthorizeURL(t *testing.T) {
	raw := buildAuthorizeURL("state-123")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("authorize URL does not parse: %v", err)
	}
	if parsed.Host != "bigmodel.cn" || parsed.Path != "/login" {
		t.Fatalf("authorize target = %s%s, want bigmodel.cn/login", parsed.Host, parsed.Path)
	}
	query := parsed.Query()
	if query.Get("redirect") != loginRedirectURI {
		t.Errorf("redirect=%q want %q", query.Get("redirect"), loginRedirectURI)
	}
	if query.Get("appId") != "zcode" {
		t.Errorf("appId=%q want zcode", query.Get("appId"))
	}
	if query.Get("state") != "state-123" {
		t.Errorf("state=%q want state-123", query.Get("state"))
	}
	// One region ships, so no authorize URL may point at the international
	// host.
	if strings.Contains(raw, "chat.z.ai") || strings.Contains(raw, "z.ai/") {
		t.Errorf("authorize URL points at the international service: %s", raw)
	}
}

func TestParseCallbackURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		state   string
		want    string
		wantErr string
	}{
		{name: "code", raw: "zcode://oauth/callback?code=abc&state=s1", state: "s1", want: "abc"},
		{name: "authCode spelling", raw: "zcode://oauth/callback?authCode=xyz&state=s1", state: "s1", want: "xyz"},
		{name: "urlencoded code", raw: "zcode://oauth/callback?code=a%2Bb%2Fc%3D&state=s1", state: "s1", want: "a+b/c="},
		{name: "browser fragment tolerated", raw: "zcode://oauth/callback?code=abc&state=s1#section", state: "s1", want: "abc"},
		{name: "international callback rejected", raw: "zcode://zai-auth/callback?code=abc&state=s1", state: "s1", wantErr: "unexpected callback target"},
		{name: "empty", raw: "   ", wantErr: "paste the zcode://"},
		{name: "wrong scheme", raw: "https://bigmodel.cn/login?code=abc&state=s1", wantErr: "zcode://"},
		{name: "wrong host", raw: "zcode://zai-auth/callback?code=abc&state=s1", wantErr: "unexpected callback target"},
		{name: "no state", raw: "zcode://oauth/callback?code=abc", wantErr: "no state"},
		{name: "state mismatch", raw: "zcode://oauth/callback?code=abc&state=other", state: "s1", wantErr: "state mismatch"},
		{name: "provider error", raw: "zcode://oauth/callback?error=access_denied&state=s1", state: "s1", wantErr: "rejected"},
		{name: "no code", raw: "zcode://oauth/callback?state=s1", state: "s1", wantErr: "no code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := parseCallbackURL(tc.raw, tc.state)
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
				// Only the query-parse cases with a trailing fragment land
				// here; the code is whatever url.Parse recovered.
				return
			}
			if code != tc.want {
				t.Fatalf("code=%q want %q", code, tc.want)
			}
		})
	}
}

func TestStartLoginReturnsAuthorizeSessionAndPendingState(t *testing.T) {
	client := newLoginClient(t, &memStore{}, "", "")
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
	done, message, err := client.PollLogin(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if done {
		t.Fatal("PollLogin reported done before any callback was pasted")
	}
	if !strings.Contains(message, "zcode://oauth/callback") {
		t.Fatalf("PollLogin message %q should tell the operator what to paste", message)
	}
}

func TestPollLoginRejectsUnknownAndExpiredSessions(t *testing.T) {
	client := newLoginClient(t, &memStore{}, "", "")
	if _, _, err := client.PollLogin(context.Background(), "missing"); err == nil {
		t.Fatal("PollLogin should fail for an account with no started login")
	}
	if err := client.CompleteLogin(context.Background(), "missing", "zcode://oauth/callback?code=a&state=s"); err == nil {
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

func TestCompleteLoginExchangesCodeAndStoresDomesticCredential(t *testing.T) {
	jwt := jwtForTest(t, map[string]any{"user_id": "u-1", "provider": "bigmodel"})
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
		_, _ = w.Write([]byte(`{"code":0,"data":{"token":"` + jwt + `","bigmodel":{"access_token":"at-1","refresh_token":"rt-1"},"expires_in":3600}}`))
	}))
	defer tokenServer.Close()

	var identityAuth string
	identityServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identityAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":{"user_id":"u-1","email":"domestic@example.com"}}`))
	}))
	defer identityServer.Close()

	store := &memStore{}
	client := newLoginClient(t, store, tokenServer.URL, identityServer.URL)

	session, err := client.StartLogin(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	callback := "zcode://oauth/callback?code=CODE-1&state=" + url.QueryEscape(session.State)
	if err := client.CompleteLogin(context.Background(), "acc1", callback); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	if tokenBody["provider"] != RegionBigModel {
		t.Errorf("exchange provider=%q want %q", tokenBody["provider"], RegionBigModel)
	}
	if tokenBody["code"] != "CODE-1" {
		t.Errorf("exchange code=%q want CODE-1", tokenBody["code"])
	}
	if tokenBody["redirect_uri"] != loginRedirectURI {
		t.Errorf("exchange redirect_uri=%q want %q", tokenBody["redirect_uri"], loginRedirectURI)
	}
	if tokenBody["state"] != session.State {
		t.Errorf("exchange state=%q want %q", tokenBody["state"], session.State)
	}
	if identityAuth != "at-1" {
		t.Errorf("identity Authorization=%q want the raw access token", identityAuth)
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
	if credential.ExpiresAt <= time.Now().Unix() {
		t.Errorf("ExpiresAt=%d want a future timestamp from expires_in", credential.ExpiresAt)
	}
	// The round is consumed: replaying the same callback must fail.
	if err := client.CompleteLogin(context.Background(), "acc1", callback); err == nil {
		t.Fatal("replaying a consumed callback should fail")
	}
}

func TestCompleteLoginRejectsStateFromAnotherRound(t *testing.T) {
	tokenCalls := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		_, _ = w.Write([]byte(`{"code":0,"data":{"token":"jwt"}}`))
	}))
	defer tokenServer.Close()

	store := &memStore{}
	client := newLoginClient(t, store, tokenServer.URL, "")
	if _, err := client.StartLogin(context.Background(), "acc1"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	err := client.CompleteLogin(context.Background(), "acc1", "zcode://oauth/callback?code=c&state=stale")
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

	client := newLoginClient(t, &memStore{}, tokenServer.URL, "")
	session, err := client.StartLogin(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	err = client.CompleteLogin(context.Background(), "acc1", "zcode://oauth/callback?code=c&state="+session.State)
	if err == nil {
		t.Fatal("CompleteLogin should fail when the token endpoint rejects the code")
	}
	if !strings.Contains(err.Error(), "3001") && !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("error %q should carry the upstream failure", err.Error())
	}
}

func TestExchangeRejectsMissingJWT(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"bigmodel":{"access_token":"at"}}}`))
	}))
	defer tokenServer.Close()

	client := newLoginClient(t, &memStore{}, tokenServer.URL, "")
	if _, err := client.exchangeAuthorizationCode(context.Background(), "c", "s"); err == nil {
		t.Fatal("exchange should fail when data.token is absent")
	}
}

func TestZCodeAdapterExposesBrowserLogin(t *testing.T) {
	client := NewClient(&memStore{})
	adapter := client.Adapter()
	if !adapter.Supports("login") {
		t.Fatal("zcode adapter must expose the login capability")
	}
	if _, ok := adapter.Login.(providers.LoginCompleter); !ok {
		t.Fatal("zcode login must accept a pasted callback URL (LoginCompleter)")
	}
}

func TestProviderDescriptorShipsDomesticRegionOnly(t *testing.T) {
	if len(providers.ZCode.Regions) != 1 {
		t.Fatalf("zcode regions=%d want 1 (domestic only)", len(providers.ZCode.Regions))
	}
	region := providers.ZCode.Regions[0]
	if region.ID != RegionBigModel {
		t.Errorf("region=%q want %q", region.ID, RegionBigModel)
	}
	if providers.ZCode.DefaultRegion != RegionBigModel {
		t.Errorf("default region=%q want %q", providers.ZCode.DefaultRegion, RegionBigModel)
	}
	if !providers.ZCode.Capabilities.BrowserLogin || !providers.ZCode.Capabilities.Login {
		t.Error("zcode descriptor must advertise the browser login")
	}
	if _, ok := providers.ZCode.Region(RegionZAI); ok {
		t.Error("the international region must not be advertised")
	}
}

func TestImportRejectsInternationalCredential(t *testing.T) {
	payload := []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"zai","api_key":"abc.def"}`)
	if err := ValidateCredential(payload); err == nil {
		t.Fatal("ValidateCredential must reject an international (Z.ai) credential")
	} else if !strings.Contains(err.Error(), "international") {
		t.Fatalf("error %q should explain that only the domestic service ships", err.Error())
	}
	if _, err := (credentialCodec{}).PrepareImport(payload); err == nil {
		t.Fatal("PrepareImport must reject an international (Z.ai) credential")
	}
	domestic := []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"bigmodel","api_key":"k1"}`)
	if err := ValidateCredential(domestic); err != nil {
		t.Fatalf("ValidateCredential(domestic): %v", err)
	}
}
