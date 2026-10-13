package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestLoginRoundTripThroughLoopback drives a full browser-login round with a
// stubbed token endpoint: StartLogin mints the state, the loopback callback is
// invoked with that state and a code, and the credential lands in the store.
func TestLoginRoundTripThroughLoopback(t *testing.T) {
	var sawCode, sawRedirect string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sawCode = r.PostForm.Get("code")
		sawRedirect = r.PostForm.Get("redirect_uri")
		if r.PostForm.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", r.PostForm.Get("grant_type"))
		}
		_, _ = w.Write([]byte(`{"access_token":"ya29.acc","refresh_token":"1//0g-new","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokenServer.Close()
	withTokenEndpoint(t, tokenServer.URL)

	store := &fakeStore{}
	client := NewClient(store)
	session, err := client.StartLogin(context.Background(), "acct-loop")
	if err != nil {
		t.Fatal(err)
	}
	if session.State == "" {
		t.Fatal("login round must mint a state")
	}
	redirect, err := client.redirectURI()
	if err != nil {
		t.Fatal(err)
	}

	// The loopback handler receives the full callback URL as the browser sent it.
	callback := redirect + "?code=the-code&state=" + url.QueryEscape(session.State)
	if err := client.acceptCallback(context.Background(), callback); err != nil {
		t.Fatalf("acceptCallback: %v", err)
	}
	if sawCode != "the-code" {
		t.Fatalf("token endpoint saw code %q", sawCode)
	}
	// The redirect_uri echoed to the token endpoint must be byte-identical to
	// the one used to build the authorize URL.
	if sawRedirect != redirect {
		t.Fatalf("redirect_uri = %q, want %q", sawRedirect, redirect)
	}
	if !strings.Contains(session.AuthURL, url.QueryEscape(redirect)) {
		t.Fatalf("authorize URL must carry the same redirect: %s", session.AuthURL)
	}

	done, message, err := client.PollLogin(context.Background(), "acct-loop")
	if err != nil {
		t.Fatalf("poll after a completed round: %v", err)
	}
	if !done {
		t.Fatalf("round must report done, message=%q", message)
	}

	stored, err := DecodeCredential(store.payloads["acct-loop"])
	if err != nil {
		t.Fatal(err)
	}
	if !stored.IsOAuth() {
		t.Fatalf("stored credential is not oauth: %+v", stored)
	}
	if stored.RefreshToken != "1//0g-new" {
		t.Fatalf("refresh token = %q", stored.RefreshToken)
	}
	if stored.Format != CredentialFormatOAuth {
		t.Fatalf("format = %q", stored.Format)
	}
}

// TestLoopbackRejectsUnknownState makes sure a callback carrying a state this
// process never minted cannot complete anything.
func TestLoopbackRejectsUnknownState(t *testing.T) {
	store := &fakeStore{}
	client := NewClient(store)
	if _, err := client.StartLogin(context.Background(), "acct-x"); err != nil {
		t.Fatal(err)
	}
	err := client.acceptCallback(context.Background(), "http://127.0.0.1:1/oauth-callback?code=c&state=forged")
	if err == nil {
		t.Fatal("a forged state must be rejected")
	}
	if len(store.payloads) != 0 {
		t.Fatal("nothing may be stored for a forged callback")
	}
}

// TestLoginWithoutRefreshTokenIsRejected covers the case Google answers a repeat
// authorization: no refresh token comes back, and that account would have no
// renewal path, so it must not be stored silently.
func TestLoginWithoutRefreshTokenIsRejected(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"ya29.acc","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	withTokenEndpoint(t, tokenServer.URL)

	store := &fakeStore{}
	client := NewClient(store)
	session, err := client.StartLogin(context.Background(), "acct-no-refresh")
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := client.redirectURI()
	if err != nil {
		t.Fatal(err)
	}
	callback := redirect + "?code=c&state=" + session.State
	err = client.CompleteLogin(context.Background(), "acct-no-refresh", callback)
	if err == nil {
		t.Fatal("a login without a refresh token must fail")
	}
	if !strings.Contains(err.Error(), "refresh token") {
		t.Fatalf("error should explain the missing refresh token: %v", err)
	}
	if len(store.payloads) != 0 {
		t.Fatal("an unusable credential must not be stored")
	}
}

// TestRenewCredentialPersistsNewAccessToken checks the refresh write-back path.
func TestRenewCredentialPersistsNewAccessToken(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"ya29.renewed","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	withTokenEndpoint(t, tokenServer.URL)

	store := &fakeStore{}
	seed := Credential{
		Format:       CredentialFormatOAuth,
		AuthMode:     AuthModeOAuth,
		RefreshToken: "1//0g-seed",
		AccessToken:  "ya29.stale",
		// Already past the renewal window, so freshCredential renews it.
		ExpiresAt: 1,
	}
	payload, err := seed.Encode()
	if err != nil {
		t.Fatal(err)
	}
	store.payloads = map[string][]byte{"acct-renew": payload}

	client := NewClient(store)
	fresh, err := client.freshCredential(context.Background(), "acct-renew")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.AccessToken != "ya29.renewed" {
		t.Fatalf("access token = %q", fresh.AccessToken)
	}
	stored, err := DecodeCredential(store.payloads["acct-renew"])
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "ya29.renewed" {
		t.Fatalf("renewed token was not written back: %q", stored.AccessToken)
	}
	if stored.RefreshToken != "1//0g-seed" {
		t.Fatalf("refresh token must be preserved: %q", stored.RefreshToken)
	}
}

// TestRenewCredentialDoesNotClobberNewerCredential covers compare-before-write:
// the operator re-logs-in while a refresh is in flight, and the older refresh
// must not overwrite the newer credential.
func TestRenewCredentialDoesNotClobberNewerCredential(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"ya29.renewed","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	withTokenEndpoint(t, tokenServer.URL)

	store := &fakeStore{}
	client := NewClient(store)
	stale := Credential{
		Format:       CredentialFormatOAuth,
		AuthMode:     AuthModeOAuth,
		RefreshToken: "1//0g-old",
		AccessToken:  "ya29.stale",
		ExpiresAt:    1,
	}
	// Simulate the re-login landing before the refresh write-back.
	newer := Credential{
		Format:       CredentialFormatOAuth,
		AuthMode:     AuthModeOAuth,
		RefreshToken: "1//0g-newer",
		AccessToken:  "ya29.newer",
		ExpiresAt:    9999999999999,
	}
	newerPayload, err := newer.Encode()
	if err != nil {
		t.Fatal(err)
	}
	store.payloads = map[string][]byte{"acct-race": newerPayload}

	// The refresh starts from the stale credential but the store already holds
	// the newer one, so nothing may be written back.
	if _, err := client.refreshAndStore(context.Background(), "acct-race", stale); err != nil {
		t.Fatal(err)
	}
	stored, err := DecodeCredential(store.payloads["acct-race"])
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "1//0g-newer" {
		t.Fatalf("the newer credential was clobbered: %q", stored.RefreshToken)
	}
}
