package raccoon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// loginTestClient wires a Client to a stub auth server. It reuses the package's
// memStore so the assertions can read back what the login path stored.
func loginTestClient(t *testing.T, authHandler http.HandlerFunc) (*Client, *memStore) {
	t.Helper()
	auth := httptest.NewServer(authHandler)
	t.Cleanup(auth.Close)
	store := &memStore{items: map[string][]byte{}}
	client := NewClient(store)
	client.authBase = auth.URL
	client.siteBase = auth.URL
	return client, store
}

// TestStartLoginBuildsDesktopAuthorizeURL pins the authorize shape: the state
// must be echoed and the appname (Chinese) must survive percent-encoding.
func TestStartLoginBuildsDesktopAuthorizeURL(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	session, err := client.StartLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if !strings.Contains(session.AuthURL, authorizePath) {
		t.Fatalf("authorize url missing path: %s", session.AuthURL)
	}
	if !strings.Contains(session.AuthURL, "login_source=desktop") {
		t.Fatalf("authorize url missing login_source: %s", session.AuthURL)
	}
	if !strings.Contains(session.AuthURL, "state="+session.State) {
		t.Fatalf("authorize url missing state: %s", session.AuthURL)
	}
	// The Chinese app name must be encoded, never pasted raw.
	if strings.Contains(session.AuthURL, appName) {
		t.Fatalf("appname was not percent-encoded: %s", session.AuthURL)
	}
	if session.State == "" {
		t.Fatal("state must be minted")
	}
}

// TestPollBeforeCompletionReportsPending covers the "waiting" branch.
func TestPollBeforeCompletionReportsPending(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	if _, err := client.StartLogin(context.Background(), "acc-1"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	done, message, err := client.PollLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if done {
		t.Fatal("round must not report done before the callback arrives")
	}
	if !strings.Contains(message, "office-raccoon") {
		t.Fatalf("pending message should explain the custom-scheme callback: %q", message)
	}
}

// TestPollWithoutStartIsAnError covers polling an account that never started.
func TestPollWithoutStartIsAnError(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	if _, _, err := client.PollLogin(context.Background(), "nobody"); err == nil {
		t.Fatal("polling an account with no round must fail")
	}
}

// TestCompleteLoginRejectsForgedState is the security assertion: a callback
// whose state this process never minted must not spend the code, so an attacker
// cannot hand back somebody else's code and have it stored as this account.
func TestCompleteLoginRejectsForgedState(t *testing.T) {
	called := false
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {
		called = true
	})
	if _, err := client.StartLogin(context.Background(), "acc-1"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	forged := "office-raccoon://auth/callback?code=stolen&state=forged"
	if err := client.CompleteLogin(context.Background(), "acc-1", forged); err == nil {
		t.Fatal("a forged state must be rejected")
	}
	if called {
		t.Fatal("the exchange must not be attempted for a rejected callback")
	}
}

// TestCompleteLoginRejectsWrongScheme covers the shape check: a prefix match
// would wrongly accept these.
func TestCompleteLoginRejectsWrongScheme(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	session, err := client.StartLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	for _, bad := range []string{
		"office-raccoon://auth/callback@evil.example?code=x&state=" + session.State,
		"office-raccoon://auth/callback.evil?code=x&state=" + session.State,
		"https://xiaohuanxiong.com/code/authorize?code=x&state=" + session.State,
	} {
		if err := client.CompleteLogin(context.Background(), "acc-1", bad); err == nil {
			t.Fatalf("callback shape must be rejected: %s", bad)
		}
	}
}

// TestCompleteLoginStoresCredentialAndMarksDone covers the happy path end to
// end: a valid callback exchanges the code, stores the pair, and the poll
// reports done exactly once.
func TestCompleteLoginStoresCredentialAndMarksDone(t *testing.T) {
	var gotCode string
	client, store := loginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, exchangePath) {
			http.NotFound(w, r)
			return
		}
		var body struct {
			AuthorizationCode string `json:"authorization_code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotCode = body.AuthorizationCode
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"access-1","refresh_token":"refresh-1","user_id":"u-9"}}`))
	})
	session, err := client.StartLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	callback := "office-raccoon://auth/callback?code=code-1&state=" + session.State
	if err := client.CompleteLogin(context.Background(), "acc-1", callback); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if gotCode != "code-1" {
		t.Fatalf("exchange used the wrong code: %q", gotCode)
	}
	if store.formats["acc-1"] != CredentialFormat {
		t.Fatalf("stored format = %q", store.formats["acc-1"])
	}
	cred, err := DecodeCredential(store.items["acc-1"])
	if err != nil {
		t.Fatalf("stored payload is not decodable: %v", err)
	}
	if cred.AccessToken != "access-1" || cred.RefreshToken != "refresh-1" || cred.UserID != "u-9" {
		t.Fatalf("stored credential = %+v", cred)
	}
	if store.observed["acc-1"] != "ready" {
		t.Fatalf("account not marked ready: %q", store.observed["acc-1"])
	}

	// The round is reported done exactly once, then cleared.
	done, _, err := client.PollLogin(context.Background(), "acc-1")
	if err != nil || !done {
		t.Fatalf("first poll after login: done=%v err=%v", done, err)
	}
	if _, _, err := client.PollLogin(context.Background(), "acc-1"); err == nil {
		t.Fatal("the round must be cleared after being reported")
	}
}

// TestCompleteLoginReportsExpiredCode covers the 200035 branch: retrying the
// same link cannot help, so the message must say to start over.
func TestCompleteLoginReportsExpiredCode(t *testing.T) {
	client, store := loginTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":200035,"message":"authorization code expired"}`))
	})
	session, err := client.StartLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	callback := "office-raccoon://auth/callback?code=old&state=" + session.State
	err = client.CompleteLogin(context.Background(), "acc-1", callback)
	if err == nil {
		t.Fatal("an expired code must fail")
	}
	if !strings.Contains(err.Error(), "重新发起") {
		t.Fatalf("expired-code message should tell the operator to start over: %v", err)
	}
	if store.items["acc-1"] != nil {
		t.Fatal("nothing may be stored for a failed exchange")
	}
}

// TestCompleteLoginAcceptsRefreshTokenOnlyExchange pins that the credential is
// Ready from a refresh token alone (the first call mints the access token).
func TestCompleteLoginAcceptsRefreshTokenOnlyExchange(t *testing.T) {
	client, store := loginTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"refresh_token":"refresh-only"}}`))
	})
	session, err := client.StartLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	callback := "office-raccoon://auth/callback?code=c&state=" + session.State
	if err := client.CompleteLogin(context.Background(), "acc-1", callback); err != nil {
		t.Fatalf("a refresh-token-only exchange is usable: %v", err)
	}
	cred, err := DecodeCredential(store.items["acc-1"])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cred.Ready() || cred.AccessToken != "" || cred.RefreshToken != "refresh-only" {
		t.Fatalf("credential = %+v", cred)
	}
}

// TestCompleteLoginRejectsEmptyExchange covers a 200 with no tokens at all.
func TestCompleteLoginRejectsEmptyExchange(t *testing.T) {
	client, store := loginTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	})
	session, err := client.StartLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	callback := "office-raccoon://auth/callback?code=c&state=" + session.State
	if err := client.CompleteLogin(context.Background(), "acc-1", callback); err == nil {
		t.Fatal("an exchange with no tokens must fail")
	}
	if store.items["acc-1"] != nil {
		t.Fatal("nothing may be stored for a failed exchange")
	}
}

// TestCompleteLoginWithoutRoundIsAnError covers pasting a callback before
// starting a login.
func TestCompleteLoginWithoutRoundIsAnError(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	callback := "office-raccoon://auth/callback?code=c&state=whatever"
	if err := client.CompleteLogin(context.Background(), "acc-1", callback); err == nil {
		t.Fatal("a callback with no round must be rejected")
	}
}

// TestImporterAcceptsAccessTokenAndAuthJSON pins both paste routes the console
// offers: a bare access token and the desktop client's auth.json.
func TestImporterAcceptsAccessTokenAndAuthJSON(t *testing.T) {
	codec := credentialCodec{}
	if codec.Format() != CredentialFormat {
		t.Fatalf("format = %q", codec.Format())
	}
	for name, payload := range map[string]string{
		"canonical":   `{"access_token":"a","refresh_token":"r"}`,
		"camel":       `{"accessToken":"a","refreshToken":"r"}`,
		"token":       `{"token":"a"}`,
		"api_key":     `{"api_key":"a"}`,
		"bare string": `"a"`,
	} {
		prepared, err := codec.PrepareImport([]byte(payload))
		if err != nil {
			t.Fatalf("%s: PrepareImport: %v", name, err)
		}
		if !prepared.Ready {
			t.Fatalf("%s: payload should be ready", name)
		}
		cred, err := DecodeCredential(prepared.Payload)
		if err != nil {
			t.Fatalf("%s: decode normalized payload: %v", name, err)
		}
		if cred.AccessToken != "a" {
			t.Fatalf("%s: access token = %q", name, cred.AccessToken)
		}
	}
}

// TestImporterRejectsEmptyPayload guards against storing an unusable account.
func TestImporterRejectsEmptyPayload(t *testing.T) {
	codec := credentialCodec{}
	if _, err := codec.PrepareImport([]byte(`{}`)); err == nil {
		t.Fatal("an empty credential must be rejected")
	}
}

// TestAdapterExposesBrowserLogin pins the capability wiring the console reads:
// the wizard only renders the pasted-callback box when these are true.
func TestAdapterExposesBrowserLogin(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	adapter := client.Adapter()
	if adapter.Login == nil {
		t.Fatal("raccoon must expose a login provider")
	}
	if _, ok := adapter.Login.(providers.LoginCompleter); !ok {
		t.Fatal("raccoon login must accept a pasted callback URL")
	}
	if _, ok := adapter.Credential.(providers.CredentialImporter); !ok {
		t.Fatal("raccoon credential must support import")
	}
	if adapter.ImportExport == nil {
		t.Fatal("raccoon must expose import/export")
	}
}
