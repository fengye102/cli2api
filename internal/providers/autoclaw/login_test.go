package autoclaw

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// fakeJWT builds an unsigned token with the claims the adapter reads. The
// signature is never verified on our side - the upstream is the authority - so
// a well-formed payload is all these tests need.
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func newLoginClient(t *testing.T, server *httptest.Server, region Region) (*Client, *memoryStore) {
	t.Helper()
	store := &memoryStore{
		account: accounts.Account{ID: "acct-1", Provider: region.ProviderID(), ProviderRegion: "cn-north-4"},
	}
	client := NewClient(store, region)
	client.upstreamBase = server.URL + "/autoclaw-proxy/proxy/autoclaw"
	client.userapiBase = server.URL
	client.nowFn = func() time.Time { return time.Now() }
	return client, store
}

// TestLoginSMSRoundSendsPhoneThenExchangesCode walks the domestic round the way
// the console does: start, paste the phone, paste the code.
//
// Three details here are load-bearing and were wrong in every naive port: the
// phone goes up with source_id/device_id, the code goes up as a NUMBER (the
// upstream type-checks it and answers 400001 for a string), and the login path
// keeps its trailing slash (without it: 404).
func TestLoginSMSRoundSendsPhoneThenExchangesCode(t *testing.T) {
	expiry := time.Now().Add(23 * time.Hour).Unix()
	access := fakeJWT(t, map[string]any{"exp": expiry, "user_id": "user-42"})

	var sentPhone, loginPhone string
	var loginCode any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/userapi/v1/agent-send-code":
			sentPhone, _ = body["phone"].(string)
			if body["source_id"] != "autoclaw" || body["device_id"] == "" {
				t.Errorf("sms send-code is missing the client identity: %v", body)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case "/userapi/v1/agent-login":
			loginPhone, _ = body["phone"].(string)
			loginCode = body["code"]
			resp := fmt.Sprintf(`{"code":0,"data":{"access_token":%q,"refresh_token":"REFRESH-NEW","user_id":"user-42"}}`, access)
			_, _ = w.Write([]byte(resp))
		default:
			t.Errorf("unexpected upstream call %s", r.URL.Path)
			_, _ = w.Write([]byte(`{"code":404,"msg":"no such path"}`))
		}
	}))
	defer server.Close()

	client, store := newLoginClient(t, server, RegionCN)
	flow := loginFlow{client: client}

	session, err := flow.StartLogin(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if session.State != "sms" {
		t.Fatalf("expected the sms round, got %q", session.State)
	}
	_, message, err := flow.PollLogin(context.Background(), "acct-1")
	if err != nil || !strings.Contains(message, "手机号") {
		t.Fatalf("poll should ask for the phone number: %q (%v)", message, err)
	}

	if err := flow.CompleteLogin(context.Background(), "acct-1", "138 0013 8000"); err != nil {
		t.Fatalf("CompleteLogin(phone): %v", err)
	}
	if sentPhone != "13800138000" {
		t.Fatalf("phone was not normalized before it went upstream: %q", sentPhone)
	}
	if len(store.saved) != 0 {
		t.Fatalf("sending the code must not store a credential yet: %s", store.saved)
	}
	_, message, _ = flow.PollLogin(context.Background(), "acct-1")
	if !strings.Contains(message, "8000") || strings.Contains(message, "13800138000") {
		t.Fatalf("poll should ask for the code and mask the number: %q", message)
	}

	if err := flow.CompleteLogin(context.Background(), "acct-1", "验证码 123456"); err != nil {
		t.Fatalf("CompleteLogin(code): %v", err)
	}
	if loginPhone != "13800138000" {
		t.Fatalf("code exchange used the wrong phone: %q", loginPhone)
	}
	if number, ok := loginCode.(float64); !ok || number != 123456 {
		t.Fatalf("the SMS code must go up as a number, got %T %v", loginCode, loginCode)
	}

	saved, err := DecodeCredential(store.saved)
	if err != nil {
		t.Fatalf("stored credential does not decode: %v", err)
	}
	if saved.AccessToken != access || saved.RefreshToken != "REFRESH-NEW" {
		t.Fatalf("stored credential = %+v", saved)
	}
	if saved.UserID != "user-42" {
		t.Fatalf("user id should come from the token: %q", saved.UserID)
	}
	if saved.ExpiresAt == "" {
		t.Fatal("expiry must be derived from the token, or the account never refreshes proactively")
	}
	if saved.Region != "cn" || saved.Format != CredentialFormat {
		t.Fatalf("stored credential has the wrong shape: %+v", saved)
	}
	if saved.DeviceID != client.loginDeviceID("acct-1") {
		t.Fatalf("the device id must be the stable per-account one, got %q", saved.DeviceID)
	}
	if len(store.observed) == 0 || store.observed[len(store.observed)-1] != "ready" {
		t.Fatalf("a finished login must mark the account ready: %v", store.observed)
	}
}

// TestLoginFollowsOneMovedEndpoint: the gateway answers the older
// "/agent-login/" shape with a 307 to the slashless path (verified live). This
// client does not follow redirects on the chat path, so the login path has to
// take exactly one hop by hand - otherwise a moved endpoint reads as a failed
// login with a non-JSON body.
func TestLoginFollowsOneMovedEndpoint(t *testing.T) {
	var landed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userapi/v1/moved/" {
			w.Header().Set("Location", "/userapi/v1/moved")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		landed = true
		_, _ = w.Write([]byte(`{"code":0,"data":{"ok":true}}`))
	}))
	defer server.Close()

	client, _ := newLoginClient(t, server, RegionCN)
	if _, err := client.accountPost(context.Background(), "acct-1", "/userapi/v1/moved/", map[string]any{}); err != nil {
		t.Fatalf("accountPost: %v", err)
	}
	if !landed {
		t.Fatal("the redirect was not followed")
	}
}

// TestLoginIntlOAuthRoundUsesTheWhitelistedCallback pins the Intl round: the
// navigate_uri has to be one of the ports the OAuth client has whitelisted, and
// the authorize link has to land on the provider's own host.
func TestLoginIntlOAuthRoundUsesTheWhitelistedCallback(t *testing.T) {
	access := fakeJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	var navigateURI, gotCode, gotState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/userapi/overseasv1/zai-oauth-url":
			navigateURI, _ = body["navigate_uri"].(string)
			_, _ = w.Write([]byte(`{"code":0,"data":{"oauth_url":"https://chatglm.cn/passport/authorize?client_id=x"}}`))
		case "/userapi/overseasv1/zai-oauth-login":
			gotCode, _ = body["code"].(string)
			gotState, _ = body["state"].(string)
			if body["navigate_uri"] != navigateURI {
				t.Errorf("the exchange must echo the same navigate_uri: %v vs %q", body["navigate_uri"], navigateURI)
			}
			resp := fmt.Sprintf(`{"code":0,"data":{"access_token":%q,"refresh_token":"REFRESH-INTL","user_id":"u-9"}}`, access)
			_, _ = w.Write([]byte(resp))
		default:
			t.Errorf("unexpected upstream call %s", r.URL.Path)
			_, _ = w.Write([]byte(`{"code":404}`))
		}
	}))
	defer server.Close()

	client, store := newLoginClient(t, server, RegionIntl)
	flow := loginFlow{client: client}

	session, err := flow.StartLogin(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if !strings.HasPrefix(session.AuthURL, "https://chatglm.cn/") {
		t.Fatalf("authorize URL = %q", session.AuthURL)
	}
	if navigateURI != "http://localhost:18432/auth/callback-zai" {
		t.Fatalf("navigate_uri must be the whitelisted callback, got %q", navigateURI)
	}

	callback := "http://localhost:18432/auth/callback-zai?code=CODE-1&state=STATE-1"
	if err := flow.CompleteLogin(context.Background(), "acct-1", callback); err != nil {
		t.Fatalf("CompleteLogin(callback): %v", err)
	}
	if gotCode != "CODE-1" || gotState != "STATE-1" {
		t.Fatalf("exchange used code=%q state=%q", gotCode, gotState)
	}
	saved, err := DecodeCredential(store.saved)
	if err != nil {
		t.Fatalf("stored credential: %v", err)
	}
	if saved.RefreshToken != "REFRESH-INTL" || saved.Region != "intl" || saved.Format != IntlCredentialFormat {
		t.Fatalf("stored credential = %+v", saved)
	}
}

// TestLoginIntlRefusesForeignAuthorizeHost: an authorize link that is not on
// the provider's host means it was swapped, and exchanging the code with it
// would hand the account to whoever swapped it.
func TestLoginIntlRefusesForeignAuthorizeHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"oauth_url":"https://evil.example/authorize"}}`))
	}))
	defer server.Close()

	client, _ := newLoginClient(t, server, RegionIntl)
	if _, err := (loginFlow{client: client}).StartLogin(context.Background(), "acct-1"); err == nil ||
		!strings.Contains(err.Error(), "chatglm.cn") {
		t.Fatalf("a foreign authorize host must be refused, got %v", err)
	}
}

// TestLoginCaptchaCodeExplainsItself: the Intl gateway fronts the authorize
// endpoint with an Aliyun captcha, and its own message for that ("当前版本已停止
// 服务") says nothing about what to do.
func TestLoginCaptchaCodeExplainsItself(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":631002,"msg":"当前版本已停止服务"}`))
	}))
	defer server.Close()

	client, _ := newLoginClient(t, server, RegionIntl)
	_, err := (loginFlow{client: client}).StartLogin(context.Background(), "acct-1")
	if err == nil || !strings.Contains(err.Error(), "captcha") {
		t.Fatalf("expected a captcha explanation, got %v", err)
	}
}

// TestLoginPasteAfterRestartStillExchanges: the round state is in memory, so a
// restart between "open the link" and "paste it back" must not lose the paste -
// the callback URL carries the provider and the port in itself.
func TestLoginPasteAfterRestartStillExchanges(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/userapi/overseasv1/google-oauth-login" {
			t.Errorf("unexpected call %s", r.URL.Path)
		}
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["navigate_uri"] != "http://localhost:18432/auth/callback-google" {
			t.Errorf("navigate_uri = %v", body["navigate_uri"])
		}
		called = true
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"a","refresh_token":"r","user_id":"u"}}`))
	}))
	defer server.Close()

	client, store := newLoginClient(t, server, RegionIntl)
	flow := loginFlow{client: client}
	callback := "http://localhost:18432/auth/callback-google?code=C&state=S"
	if err := flow.CompleteLogin(context.Background(), "acct-1", callback); err != nil {
		t.Fatalf("CompleteLogin without an open round: %v", err)
	}
	if !called || len(store.saved) == 0 {
		t.Fatal("the paste was not exchanged")
	}
}

// TestLoginRejectsWrongPastes keeps the guidance honest: a wrong paste is an
// error with a reason, not a silent no-op.
func TestLoginRejectsWrongPastes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer server.Close()

	client, _ := newLoginClient(t, server, RegionCN)
	flow := loginFlow{client: client}
	if _, err := flow.StartLogin(context.Background(), "acct-1"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := flow.CompleteLogin(context.Background(), "acct-1", "not-a-phone"); err == nil {
		t.Fatal("a non-phone answer must be refused before it reaches the upstream")
	}
	if err := flow.CompleteLogin(context.Background(), "acct-1", "13800138000"); err != nil {
		t.Fatalf("phone: %v", err)
	}
	if err := flow.CompleteLogin(context.Background(), "acct-1", "abc"); err == nil {
		t.Fatal("a non-code answer must be refused")
	}
	if err := flow.CompleteLogin(context.Background(), "acct-1", ""); err == nil {
		t.Fatal("an empty paste must be refused")
	}
}

// TestLoginDeviceIDIsStableAndUUIDShaped: the upstream keys the refresh token
// to this id, so it has to survive restarts and stay unique per account.
func TestLoginDeviceIDIsStableAndUUIDShaped(t *testing.T) {
	first := NewClient(nil, RegionCN).loginDeviceID("acct-1")
	again := NewClient(nil, RegionCN).loginDeviceID("acct-1")
	if first != again {
		t.Fatalf("device id must be stable: %q vs %q", first, again)
	}
	if len(first) != 36 || first[14] != '4' || strings.Count(first, "-") != 4 {
		t.Fatalf("device id must be a version-4 UUID, got %q", first)
	}
	if other := NewClient(nil, RegionCN).loginDeviceID("acct-2"); other == first {
		t.Fatal("two accounts must not share a device id")
	}
	if intl := NewClient(nil, RegionIntl).loginDeviceID("acct-1"); intl == first {
		t.Fatal("the two builds are separate account planes and must not share a device id")
	}
}

func TestNormalizePhone(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"13800138000", "13800138000"},
		{"+8613800138000", "13800138000"},
		{"8613800138000", "13800138000"},
		{"138 0013 8000", "13800138000"},
		{"138-0013-8000", "13800138000"},
		{"123456", ""},
		{"23800138000", ""},
		{"", ""},
	} {
		if got := normalizePhone(tc.in); got != tc.want {
			t.Errorf("normalizePhone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSMSCodeValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"123456", "123456"},
		{"验证码 123456", "123456"},
		{"code: 654321.", "654321"},
		{"12345", ""},
		{"abcdef", ""},
		{"", ""},
	} {
		if got := smsCodeValue(tc.in); got != tc.want {
			t.Errorf("smsCodeValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLoginInputRidesOnTheContext covers the plumbing the console uses to hand
// a phone number to a provider without widening the login interface.
func TestLoginInputRidesOnTheContext(t *testing.T) {
	if got := providers.LoginInputFromContext(context.Background()); got != (providers.LoginInput{}) {
		t.Fatalf("empty context should yield the zero value, got %+v", got)
	}
	ctx := providers.WithLoginInput(context.Background(), providers.LoginInput{Phone: "13800138000", Captcha: "proof"})
	if got := providers.LoginInputFromContext(ctx); got.Phone != "13800138000" || got.Captcha != "proof" {
		t.Fatalf("login input did not survive the context: %+v", got)
	}
}
