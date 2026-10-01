package zcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// observeStore extends memStore to capture Observe calls so the tests can
// assert the auth-failure classification side effect.
type observeStore struct {
	memStore
	mu       sync.Mutex
	observed []observeCall
}

type observeCall struct {
	id, uid, status, lastError, lastKind string
}

func (s *observeStore) Observe(ctx context.Context, id, remoteUID, status, lastError, lastKind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = append(s.observed, observeCall{
		id: id, uid: remoteUID, status: status, lastError: lastError, lastKind: lastKind,
	})
	return nil
}

func (s *observeStore) lastObserved() (observeCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.observed) == 0 {
		return observeCall{}, false
	}
	return s.observed[len(s.observed)-1], true
}

// newProbeClient points the client's HTTP transport at the test server and
// overrides the balance URL through catalogURL.
func newProbeClient(t *testing.T, handler http.Handler, store *observeStore) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(store)
	client.http = server.Client()
	client.http.Transport = rewriteTransport{server: server.URL, base: client.http.Transport}
	client.catalogURL = server.URL
	return client
}

func TestProbe_Balance200_OAuth(t *testing.T) {
	var sawAuth, sawAPIKey, sawVersion string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/zcode-plan/billing/balance") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sawAuth = r.Header.Get("Authorization")
		sawAPIKey = r.Header.Get("x-api-key")
		sawVersion = r.URL.Query().Get("app_version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"balance":42.5,"currency":"CNY","plan":"pro"}}`))
	})
	store := &observeStore{memStore: memStore{items: map[string][]byte{
		"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"oauth","provider":"zai","zcode_jwt_token":"jwt-abc","email":"u@example.com"}`),
	}}}
	client := newProbeClient(t, handler, store)

	health, err := client.Probe(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !health.Ready {
		t.Errorf("Ready=false; LastError=%q", health.LastError)
	}
	if health.UID != "u@example.com" {
		t.Errorf("UID=%q", health.UID)
	}
	if sawAuth != "Bearer jwt-abc" {
		t.Errorf("Authorization=%q", sawAuth)
	}
	if sawAPIKey != "jwt-abc" {
		t.Errorf("x-api-key=%q", sawAPIKey)
	}
	if sawVersion != Version {
		t.Errorf("app_version=%q want %q", sawVersion, Version)
	}

	// Quota should parse the balance payload.
	info, err := client.Quota(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if info == nil {
		t.Fatalf("Quota returned nil")
	}
	if info.Remaining != 42.5 {
		t.Errorf("Remaining=%v", info.Remaining)
	}
	if info.Unit != "CNY" {
		t.Errorf("Unit=%q", info.Unit)
	}
	if info.Plan != "pro" {
		t.Errorf("Plan=%q", info.Plan)
	}
	if info.ProviderID != "zcode" {
		t.Errorf("ProviderID=%q", info.ProviderID)
	}
	if len(info.Windows) != 1 {
		t.Fatalf("Windows=%v", info.Windows)
	}
	if info.Windows[0].ID != "balance" {
		t.Errorf("window id=%q", info.Windows[0].ID)
	}
	if info.Exceeded {
		t.Errorf("Exceeded=true on positive balance")
	}
}

func TestProbe_Balance401_MarksAuthFailed(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"token expired or incorrect"}`))
	})
	store := &observeStore{memStore: memStore{items: map[string][]byte{
		"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"oauth","provider":"zai","zcode_jwt_token":"dead-jwt","user_id":"u-1"}`),
	}}}
	client := newProbeClient(t, handler, store)

	health, err := client.Probe(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if health.Ready {
		t.Errorf("Ready=true on 401; want false")
	}
	if health.LastError == "" {
		t.Errorf("LastError empty on 401")
	}
	obs, ok := store.lastObserved()
	if !ok {
		t.Fatalf("no Observe call recorded")
	}
	if obs.status != "login_required" {
		t.Errorf("Observe status=%q", obs.status)
	}
	if obs.lastKind != accounts.KindAuth {
		t.Errorf("Observe kind=%q want %q", obs.lastKind, accounts.KindAuth)
	}
	if obs.id != "acc1" {
		t.Errorf("Observe id=%q", obs.id)
	}
	if obs.uid != "u-1" {
		t.Errorf("Observe uid=%q", obs.uid)
	}

	// Refresh must surface an error so callers can classify the auth failure.
	_, refreshErr := client.Refresh(context.Background(), "acc1", Credential{
		AuthMode: AuthModeOAuth, Provider: RegionZAI, ZCodeJWT: "dead-jwt",
	})
	if refreshErr == nil {
		t.Fatalf("Refresh err nil on 401")
	}
	if !strings.Contains(refreshErr.Error(), "re-login") {
		t.Errorf("Refresh err=%q", refreshErr)
	}
}

func TestProbe_Balance401_APIKey(t *testing.T) {
	var sawAPIKey string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"令牌已过期或验证不正确","type":"401"}}`))
	})
	store := &observeStore{memStore: memStore{items: map[string][]byte{
		"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"bigmodel","api_key":"abc.def"}`),
	}}}
	client := newProbeClient(t, handler, store)

	health, err := client.Probe(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if health.Ready {
		t.Errorf("Ready=true on 401")
	}
	if sawAPIKey != "abc.def" {
		t.Errorf("x-api-key=%q", sawAPIKey)
	}
	obs, ok := store.lastObserved()
	if !ok || obs.lastKind != accounts.KindAuth {
		t.Errorf("Observe=%+v ok=%v", obs, ok)
	}
}

func TestQuota_DegradesOnMissingBalance(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Empty data: balance/used/total all missing.
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	})
	store := &observeStore{memStore: memStore{items: map[string][]byte{
		"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"zai","api_key":"k"}`),
	}}}
	client := newProbeClient(t, handler, store)
	info, err := client.Quota(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if info != nil {
		t.Errorf("info=%v want nil on empty payload", info)
	}
}
