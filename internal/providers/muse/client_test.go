package muse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

type stubStore struct {
	payload  []byte
	saved    []byte
	lastKind string
	lastErr  string
	status   string
}

func (s *stubStore) Get(ctx context.Context, id string) (accounts.Account, error) {
	return accounts.Account{}, accounts.ErrAccountNotFound
}

func (s *stubStore) LoadCredentialPayload(ctx context.Context, accountID string) (string, []byte, error) {
	return CredentialFormat, s.payload, nil
}

func (s *stubStore) SaveCredentialPayload(ctx context.Context, accountID, format string, payload []byte) error {
	if format != CredentialFormat {
		return nil
	}
	s.saved = payload
	return nil
}

func (s *stubStore) Observe(ctx context.Context, id, remoteUID, status, lastError, lastKind string) error {
	s.status = status
	s.lastErr = lastError
	s.lastKind = lastKind
	return nil
}

func newTestClient(t *testing.T, handler http.HandlerFunc, payload []byte) (*Client, *stubStore) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	store := &stubStore{payload: payload}
	client := NewClient(store)
	client.sessionURL = server.URL
	return client, store
}

func TestProbeReadyLabellsAccount(t *testing.T) {
	payload, err := ParseImport([]byte(`{"cookies":{"hatch_sess":"s","hatch_native_auth_device":"d"}}`))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	encoded, _ := payload.Encode()

	client, store := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "hatch_sess=s") {
			t.Errorf("probe sent Cookie %q, want the imported session cookie", got)
		}
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("probe sent User-Agent %q, want the desktop Chrome UA", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"email":"fengye@example.com","id":"u_1"}}`))
	}, encoded)

	health, err := client.Probe(context.Background(), "acc_1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !health.Ready || !health.Hot {
		t.Fatalf("health = %+v, want ready+hot", health)
	}
	if health.UID != "fengye@example.com" {
		t.Fatalf("health.UID = %q, want the account email", health.UID)
	}
	if store.status != "ready" {
		t.Fatalf("observed status %q, want ready", store.status)
	}
	if len(store.saved) == 0 {
		t.Fatal("probe did not persist the discovered identity")
	}
	var saved Credential
	if err := json.Unmarshal(store.saved, &saved); err != nil {
		t.Fatalf("saved credential: %v", err)
	}
	if saved.Email != "fengye@example.com" {
		t.Fatalf("saved email = %q", saved.Email)
	}
}

func TestProbeDetectsExpiredSession(t *testing.T) {
	payload, _ := ParseImport([]byte(`{"cookies":{"hatch_sess":"s"}}`))
	encoded, _ := payload.Encode()
	client, store := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}, encoded)

	health, err := client.Probe(context.Background(), "acc_1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if health.Ready {
		t.Fatal("an expired session must not be reported ready")
	}
	if store.lastKind != accounts.KindAuth {
		t.Fatalf("observed kind %q, want auth", store.lastKind)
	}
	if !strings.Contains(health.LastError, "expired") {
		t.Fatalf("LastError = %q, want it to mention expiry", health.LastError)
	}
}

func TestProbeDetectsLoggedOutPayload(t *testing.T) {
	payload, _ := ParseImport([]byte(`{"cookies":{"hatch_sess":"s"}}`))
	encoded, _ := payload.Encode()
	client, store := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"kind":"loggedOut"}`))
	}, encoded)

	health, err := client.Probe(context.Background(), "acc_1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if health.Ready {
		t.Fatal("a loggedOut payload must not be reported ready")
	}
	if store.lastKind != accounts.KindAuth {
		t.Fatalf("observed kind %q, want auth", store.lastKind)
	}
}

func TestProbeWithoutSessionCookieFailsFast(t *testing.T) {
	payload, _ := ParseImport([]byte(`{"cookies":{"hatch_gw":"g"}}`))
	encoded, _ := payload.Encode()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("probe must not call upstream without a session cookie")
	}, encoded)

	health, err := client.Probe(context.Background(), "acc_1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if health.Ready || !strings.Contains(health.LastError, "hatch_sess") {
		t.Fatalf("health = %+v, want a not-ready result naming hatch_sess", health)
	}
}

func TestChatFailsWithPendingTransport(t *testing.T) {
	payload, _ := ParseImport([]byte(`{"cookies":{"hatch_sess":"s"}}`))
	encoded, _ := payload.Encode()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {}, encoded)

	_, _, err := client.ChatStream(context.Background(), "acc_1", translate.ChatRequest{Model: ModelChat})
	if err == nil {
		t.Fatal("ChatStream must fail while the transport is uncalibrated")
	}
	if !strings.Contains(err.Error(), "尚未校准") {
		t.Fatalf("error = %q, want the uncalibrated message", err)
	}
}
