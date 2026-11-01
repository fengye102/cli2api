package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// TestAuthRequestsUseTheAccountProxy drives StartLogin against an auth host that
// does not resolve: only a proxy can answer it, so a request that arrives proves
// the login honoured the account's proxy_url instead of the global egress.
func TestAuthRequestsUseTheAccountProxy(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/oidc/device/authorization/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_code":"ABCD","device_code":"dev","verification_uri":"https://auth.meta/device","verification_uri_complete":"https://auth.meta/device?user_code=ABCD","expires_in":600,"interval":5}`))
	}))
	defer proxy.Close()

	store := newFakeStore()
	store.addAccount("a1")
	account := store.accounts["a1"]
	account.ProxyURL = proxy.URL
	store.accounts["a1"] = account

	client := NewClient(store)
	// .invalid never resolves: a direct request could not answer this.
	client.authHost = "http://meta-auth.invalid"

	session, err := client.StartLogin(context.Background(), "a1")
	if err != nil {
		t.Fatalf("StartLogin must go through the account proxy: %v", err)
	}
	if session.State != "ABCD" {
		t.Fatalf("session=%+v", session)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || !strings.Contains(seen[0], "meta-auth.invalid/oidc/device/authorization/") {
		t.Fatalf("proxy saw %v, want one device-authorization request", seen)
	}
}

// TestAuthRequestsPreferTheAccountOverTheGlobalProxy pins the precedence the chat
// path already had: an account proxy wins over the global one.
func TestAuthRequestsPreferTheAccountOverTheGlobalProxy(t *testing.T) {
	var (
		mu     sync.Mutex
		viaAcc int
		viaAll int
	)
	accountProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		viaAcc++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_code":"ABCD","device_code":"dev","verification_uri":"https://auth.meta/device","verification_uri_complete":"https://auth.meta/device?user_code=ABCD","expires_in":600,"interval":5}`))
	}))
	defer accountProxy.Close()
	globalProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		viaAll++
		mu.Unlock()
		http.Error(w, "the global proxy must not be used", http.StatusTeapot)
	}))
	defer globalProxy.Close()

	store := newFakeStore()
	store.addAccount("a1")
	account := store.accounts["a1"]
	account.ProxyURL = accountProxy.URL
	store.accounts["a1"] = account
	store.secrets["proxy_url"] = globalProxy.URL

	client := NewClient(store)
	client.authHost = "http://meta-auth.invalid"
	if _, err := client.StartLogin(context.Background(), "a1"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if viaAcc != 1 {
		t.Fatalf("account proxy saw %d requests, want 1", viaAcc)
	}
	if viaAll != 0 {
		t.Fatalf("global proxy saw %d requests, want 0", viaAll)
	}
}

// failingStore fails Get with a real error (not "not found").
type failingStore struct{ *fakeStore }

func (f failingStore) Get(context.Context, string) (accounts.Account, error) {
	return accounts.Account{}, errors.New("database is locked")
}

// TestProxyResolutionFailureIsNotSilent keeps a store failure from quietly
// falling back to the global egress: traffic would leave from the wrong place
// and nothing would say so.
func TestProxyResolutionFailureIsNotSilent(t *testing.T) {
	store := failingStore{fakeStore: newFakeStore()}
	store.addAccount("a1")
	client := NewClient(store)
	if _, err := client.httpClient(context.Background(), "a1"); err == nil {
		t.Fatal("a store failure must surface, not fall back to the global proxy")
	}
}

// TestMissingAccountFallsBackToTheGlobalProxy keeps the login flow working for an
// account whose row is not readable yet.
func TestMissingAccountFallsBackToTheGlobalProxy(t *testing.T) {
	store := newFakeStore()
	client := NewClient(store)
	if _, err := client.httpClient(context.Background(), "not-saved-yet"); err != nil {
		t.Fatalf("a missing account row must fall back, got %v", err)
	}
}
