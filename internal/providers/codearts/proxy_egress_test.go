package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// proxyStore is a store whose account carries an explicit proxy_url.
type proxyStore struct {
	memoryStore
}

func (s *proxyStore) Get(context.Context, string) (accounts.Account, error) {
	return s.account, nil
}

// TestTokenExchangeUsesAccountProxy pins the egress of the STS token exchange
// to the account. Huawei's cn-north-4 STS gateway drops SYN packets from some
// overseas networks; an account-scoped proxy_url must therefore apply to
// requestToken too. When it does not, the console shows a login callback that
// hangs for minutes and never returns, which is exactly the reported bug.
func TestTokenExchangeUsesAccountProxy(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "x"})
	}))
	defer server.Close()

	store := &proxyStore{}
	store.account = accounts.Account{
		ID: "acct-1", Provider: ProviderID, ProviderRegion: "cn-north-4",
		// A closed port: any attempt to use this proxy fails immediately,
		// which proves the request was routed through it.
		ProxyURL: "socks5://127.0.0.1:1",
	}
	client := NewClient(store)
	client.stsHost = server.URL

	// An empty DPoPPrivateKey makes requestToken generate a fresh JWK, so the
	// test only exercises egress selection.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.requestToken(ctx, "acct-1", map[string][]string{
		"client_id":  {ClientID},
		"grant_type": {"authorization_code"},
		"code":       {"c"},
	}, testCredential())

	if err == nil {
		t.Fatal("requestToken ignored the account proxy and reached STS directly")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("token exchange bypassed the account proxy: server received %d requests", got)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") && !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("expected a proxy egress failure, got %v", err)
	}
}

// TestTokenExchangeFallsBackWithoutAccountProxy keeps the unproxied path
// working: an account with no proxy_url still talks to STS directly.
func TestTokenExchangeFallsBackWithoutAccountProxy(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credentials":{"access_key_id":"AK","secret_access_key":"SK","security_token":"STS","expiration":"2099-01-01T00:00:00Z"}}`))
	}))
	defer server.Close()

	store := &proxyStore{}
	store.account = accounts.Account{ID: "acct-1", Provider: ProviderID, ProviderRegion: "cn-north-4"}
	client := NewClient(store)
	client.stsHost = server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.requestToken(ctx, "acct-1", map[string][]string{
		"client_id":  {ClientID},
		"grant_type": {"authorization_code"},
		"code":       {"c"},
	}, testCredential()); err != nil {
		t.Fatalf("unproxied token exchange failed: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly 1 STS request, got %d", got)
	}
}

// TestTransportHasDialTimeout pins the fail-fast guarantee. A blocked egress
// used to burn kernel TCP retries (~2 minutes) and surface as a silent hang;
// a bounded dialer turns it into an explicit error instead.
func TestTransportHasDialTimeout(t *testing.T) {
	client := NewClient(requiredStore{})
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", client.http.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("transport has no DialContext: a blocked route would hang on kernel TCP retries")
	}
}
