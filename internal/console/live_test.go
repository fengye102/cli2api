package console

import (
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/executor"
)

func TestBuildLiveSnapshot(t *testing.T) {
	// AccountView.InFlight is the pool's current counter (Acquire/Release),
	// not a probe snapshot; /api/live just sums those live values.
	views := []accounts.AccountView{
		{Account: accounts.Account{ID: "a", Enabled: true, MaxInFlight: 4}, InFlight: 4},
		{Account: accounts.Account{ID: "b", Enabled: true}, InFlight: 1}, // default 4
		{Account: accounts.Account{ID: "c", Enabled: false, MaxInFlight: 8}},
	}
	got := buildLiveSnapshot(views, 17, executor.SessionAffinityStats{}, nil, time.Unix(0, 0))
	if got.InFlight != 5 || got.Capacity != 8 || got.RPM != 17 {
		t.Fatalf("snapshot = %+v", got)
	}
	if len(got.Accounts) != 3 || got.Accounts[1].MaxInFlight != accounts.DefaultMaxInFlight || got.Accounts[2].Enabled {
		t.Fatalf("accounts = %+v", got.Accounts)
	}
}

// TestBuildLiveSnapshotProjectsWorkers pins the card's per-account projection:
// busy slots, the moment each went busy, and the cooldown list with the time
// left to run.
func TestBuildLiveSnapshotProjectsWorkers(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	views := []accounts.AccountView{
		{
			Account:  accounts.Account{ID: "a", Name: "zcode-主号", Provider: "zcode", Enabled: true, MaxInFlight: 2},
			InFlight: 1, DownUntil: now.Add(90 * time.Second).Format(time.RFC3339),
		},
		{
			Account:        accounts.Account{ID: "b", Provider: "trae", Enabled: true, MaxInFlight: 4},
			InFlight:       0,
			ModelCooldowns: map[string]string{"glm-5.3": now.Add(30 * time.Second).Format(time.RFC3339), "expired": now.Add(-time.Minute).Format(time.RFC3339)},
		},
	}
	bound := []LiveJob{
		{ID: "r1", Name: "glm-5.3-flash", AccountID: "a", StartedAt: now.Add(-25 * time.Second).Format(time.RFC3339)},
		{ID: "r2", Name: "glm-5.3-flash", AccountID: "a", StartedAt: now.Add(-5 * time.Second).Format(time.RFC3339)},
	}
	got := buildLiveSnapshot(views, 3, executor.SessionAffinityStats{Bindings: 2}, bound, now)
	if len(got.Workers) != 2 {
		t.Fatalf("workers = %+v", got.Workers)
	}
	busy := got.Workers[0]
	if !busy.Busy || !busy.RateLimited || busy.StartedAt != now.Add(-25*time.Second).Format(time.RFC3339) {
		t.Errorf("busy worker = %+v", busy)
	}
	if busy.Name != "zcode-主号" || busy.Provider != "zcode" || busy.Progress != 0.5 {
		t.Errorf("worker identity/progress = %+v", busy)
	}
	// b holds no slot: no busy-since, and only the live model cooldown shows.
	idle := got.Workers[1]
	if idle.Busy || idle.StartedAt != "" || !idle.RateLimited {
		t.Errorf("idle worker = %+v", idle)
	}
	// The account-wide cooldown first (longest remaining), then the model one;
	// an expired window is not listed at all.
	if len(got.RateLimits) != 2 {
		t.Fatalf("rate limits = %+v", got.RateLimits)
	}
	if got.RateLimits[0].AccountID != "a" || got.RateLimits[0].Model != "" || got.RateLimits[0].RemainingMs != 90_000 {
		t.Errorf("account-wide cooldown = %+v", got.RateLimits[0])
	}
	if got.RateLimits[1].Model != "glm-5.3" || got.RateLimits[1].Label != "b · glm-5.3" {
		t.Errorf("model cooldown = %+v", got.RateLimits[1])
	}
	if got.Sticky.Bindings != 2 {
		t.Errorf("sticky = %+v", got.Sticky)
	}
}
