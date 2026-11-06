package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	sqlstore "github.com/caigee-cmd/cli2api/internal/store"
)

type recordingRenewer struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recordingRenewer) RenewSession(_ context.Context, accountID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, accountID)
	return r.err
}

func (r *recordingRenewer) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// The sweep only hands enabled accounts of a registered provider to its
// renewer: a disabled account is not spending anything, and a provider that
// registered no renewer must not be touched at all.
func TestRenewProviderSessionsSkipsDisabledAndOtherProviders(t *testing.T) {
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "renew.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	manager := NewManager(ManagerConfig{DataDir: t.TempDir()}, store, nil)
	t.Cleanup(func() { manager.Close() })

	ctx := context.Background()
	loomyOn, err := store.Create(ctx, accounts.CreateAccount{Name: "on", Provider: "loomy", Region: "cn", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "off", Provider: "loomy", Region: "cn", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "other", Provider: "qoder", Region: "cn", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	renewer := &recordingRenewer{}
	manager.SetSessionRenewer("loomy", renewer)
	manager.renewProviderSessions(ctx)

	calls := renewer.snapshot()
	if len(calls) != 1 || calls[0] != loomyOn.ID {
		t.Fatalf("renewer calls = %v, want just %s", calls, loomyOn.ID)
	}
}

// A failing renewal must not stop the sweep: one broken account cannot be
// allowed to starve the others of their hourly chance.
func TestRenewProviderSessionsContinuesAfterFailure(t *testing.T) {
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "renew.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	manager := NewManager(ManagerConfig{DataDir: t.TempDir()}, store, nil)
	t.Cleanup(func() { manager.Close() })

	ctx := context.Background()
	for _, name := range []string{"a", "b"} {
		if _, err := store.Create(ctx, accounts.CreateAccount{Name: name, Provider: "loomy", Region: "cn", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}

	renewer := &recordingRenewer{err: errors.New("boom")}
	manager.SetSessionRenewer("loomy", renewer)
	manager.renewProviderSessions(ctx)

	if calls := renewer.snapshot(); len(calls) != 2 {
		t.Fatalf("renewer calls = %v, want both accounts attempted", calls)
	}
}

// No renewer registered means no work and no store round-trip.
func TestRenewProviderSessionsWithoutRenewersIsNoop(t *testing.T) {
	manager := &Manager{}
	manager.renewProviderSessions(context.Background())
}
