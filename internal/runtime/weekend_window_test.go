package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	sqlstore "github.com/caigee-cmd/cli2api/internal/store"
)

// fakeWeekendClaimer counts rounds and answers the window question.
type fakeWeekendClaimer struct {
	calls atomic.Int64
	open  bool
}

func (f *fakeWeekendClaimer) AutoClaimWeekend(context.Context, string) (providers.ClaimResult, error) {
	f.calls.Add(1)
	return providers.ClaimResult{Message: "looked"}, nil
}

func (f *fakeWeekendClaimer) WeekendClaimWindowOpen(time.Time) bool { return f.open }

func (f *fakeWeekendClaimer) PreviewClaim(context.Context, string) ([]providers.ClaimPlan, error) {
	return nil, nil
}

func (f *fakeWeekendClaimer) Claim(context.Context, string, string) (providers.ClaimResult, error) {
	return providers.ClaimResult{}, nil
}

// silentClaimer only implements the claim round: no window opinion.
type silentClaimer struct{ calls atomic.Int64 }

func (s *silentClaimer) AutoClaimWeekend(context.Context, string) (providers.ClaimResult, error) {
	s.calls.Add(1)
	return providers.ClaimResult{Message: "looked"}, nil
}

func (s *silentClaimer) PreviewClaim(context.Context, string) ([]providers.ClaimPlan, error) {
	return nil, nil
}

func (s *silentClaimer) Claim(context.Context, string, string) (providers.ClaimResult, error) {
	return providers.ClaimResult{}, nil
}

func newWeekendSweepManager(t *testing.T, claim providers.AccountClaimer) (*Manager, Account) {
	t.Helper()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "weekend.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	manager := NewManager(ManagerConfig{DataDir: t.TempDir()}, store, nil)
	t.Cleanup(func() { manager.Close() })
	registry := providers.NewRegistry()
	registry.Register(providers.Adapter{ID: weekendClaimProvider, Claim: claim})
	manager.SetProviders(registry)
	enabled := true
	account, err := store.Create(context.Background(), accounts.CreateAccount{
		Name: "zcode", Provider: weekendClaimProvider, Enabled: true, AutoCheckin: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, account
}

// TestWeekendSweepStaysQuietOutsideTheWindow is the fix for a sweep that posted
// a claim every ten minutes all week: the adapter says the window is closed, so
// no round runs and no throttle is stamped.
func TestWeekendSweepStaysQuietOutsideTheWindow(t *testing.T) {
	claimer := &fakeWeekendClaimer{open: false}
	manager, account := newWeekendSweepManager(t, claimer)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) // a Tuesday

	manager.runWeekendClaims(context.Background(), now)
	if claimer.calls.Load() != 0 {
		t.Fatalf("rounds outside the window: %d, want 0", claimer.calls.Load())
	}
	if _, stamped := weekendClaimedAt[account.ID]; stamped {
		t.Fatal("a skipped weekend must not consume the round's throttle slot")
	}
}

// TestWeekendSweepFiresInsideTheWindow is the other half: an open window runs a
// round, and the throttle then holds the next tick off.
func TestWeekendSweepFiresInsideTheWindow(t *testing.T) {
	claimer := &fakeWeekendClaimer{open: true}
	manager, _ := newWeekendSweepManager(t, claimer)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) // a Saturday

	manager.runWeekendClaims(context.Background(), now)
	if claimer.calls.Load() != 1 {
		t.Fatalf("rounds inside the window: %d, want 1", claimer.calls.Load())
	}
	manager.runWeekendClaims(context.Background(), now.Add(time.Minute))
	if claimer.calls.Load() != 1 {
		t.Fatalf("the throttle must hold the next tick off, rounds=%d", claimer.calls.Load())
	}
}

// TestWeekendSweepAcceptsAClaimerWithoutAWindow keeps older claimers working:
// no window opinion means "try", as before.
func TestWeekendSweepAcceptsAClaimerWithoutAWindow(t *testing.T) {
	claimer := &silentClaimer{}
	manager, _ := newWeekendSweepManager(t, claimer)

	manager.runWeekendClaims(context.Background(), time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	if claimer.calls.Load() != 1 {
		t.Fatalf("rounds=%d, want 1", claimer.calls.Load())
	}
}
