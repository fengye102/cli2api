package executor

import (
	"context"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

// newQuotaFailoverPool builds a pool where both regions serve the same model,
// which is the MiniMax shape: one account in cn and one in global behind the
// same public model id.
func newQuotaFailoverPool() *Pool {
	pool := NewPool(nil, nil)
	pool.Upsert(Item{
		ID:       "acc-global",
		Provider: "minimax",
		Region:   "global",
		Models:   []string{"MiniMax-M3"},
	})
	pool.Upsert(Item{
		ID:       "acc-cn",
		Provider: "minimax",
		Region:   "cn",
		Models:   []string{"MiniMax-M3"},
	})
	return pool
}

// A quota refusal on the first picked account must release the sticky region
// pin and retry on the account in the other region: the account ran out of
// credits, the model did not. Without this the request dies on
// "insufficient balance" while a healthy sibling is ready.
func TestQuotaFailoverReleasesStickyRegionPin(t *testing.T) {
	exec := ChatExecutor{Pool: newQuotaFailoverPool(), MaxAttempts: 4}
	loop := routeLoop{
		providerFilter: "minimax",
		regionFilter:   "global",
		softRegion:     true,
		attempts:       1,
		index:          1,
		excluded:       map[string]struct{}{"acc-global": {}},
	}
	quota := Classified{Kind: KindQuota, Failover: true, Cooldown: time.Hour}

	if !loop.mayFailover(exec, quota, "MiniMax-M3") {
		t.Fatal("quota refusal must fail over to the sibling account in the other region")
	}
	if loop.regionFilter != "" {
		t.Fatalf("sticky region pin was not released: %q", loop.regionFilter)
	}
	if loop.attempts != 2 {
		t.Fatalf("attempt budget must re-count the cross-region route, got %d", loop.attempts)
	}
}

// A region pinned by a model prefix or an API-key grant is a routing decision,
// not a scheduling hint: it must survive a quota refusal.
func TestQuotaFailoverKeepsHardRegionPin(t *testing.T) {
	exec := ChatExecutor{Pool: newQuotaFailoverPool(), MaxAttempts: 4}
	loop := routeLoop{
		providerFilter: "minimax",
		regionFilter:   "global",
		softRegion:     false,
		attempts:       1,
		index:          1,
		excluded:       map[string]struct{}{"acc-global": {}},
	}
	quota := Classified{Kind: KindQuota, Failover: true, Cooldown: time.Hour}

	if loop.mayFailover(exec, quota, "MiniMax-M3") {
		t.Fatal("a hard region pin must not fail over into another region")
	}
	if loop.regionFilter != "global" {
		t.Fatalf("hard region pin was released: %q", loop.regionFilter)
	}
}

// With a single account serving the model there is nowhere to fail over to, so
// the request still ends on the quota error instead of looping.
func TestQuotaFailoverStaysSingleAttemptWithoutSibling(t *testing.T) {
	pool := NewPool(nil, nil)
	pool.Upsert(Item{
		ID:       "acc-only",
		Provider: "minimax",
		Region:   "global",
		Models:   []string{"MiniMax-M3"},
	})
	exec := ChatExecutor{Pool: pool, MaxAttempts: 4}
	loop := routeLoop{
		providerFilter: "minimax",
		regionFilter:   "global",
		softRegion:     true,
		attempts:       1,
		index:          1,
		excluded:       map[string]struct{}{"acc-only": {}},
	}
	quota := Classified{Kind: KindQuota, Failover: true, Cooldown: time.Hour}

	if loop.mayFailover(exec, quota, "MiniMax-M3") {
		t.Fatal("a lone exhausted account must not stretch the attempt budget")
	}
}

// A failure kind that is not account-scoped keeps its own contract: no pin
// release, no extra attempt.
func TestQuotaPinReleaseOnlyAppliesToQuota(t *testing.T) {
	exec := ChatExecutor{Pool: newQuotaFailoverPool(), MaxAttempts: 4}
	loop := routeLoop{
		providerFilter: "minimax",
		regionFilter:   "global",
		softRegion:     true,
		attempts:       1,
		index:          1,
		excluded:       map[string]struct{}{"acc-global": {}},
	}
	invalid := Classified{Kind: KindInvalidRequest, Failover: false}

	if loop.mayFailover(exec, invalid, "MiniMax-M3") {
		t.Fatal("a request-level rejection must not fail over")
	}
	if loop.regionFilter != "global" || loop.attempts != 1 {
		t.Fatalf("a request-level rejection must not widen the route: region=%q attempts=%d", loop.regionFilter, loop.attempts)
	}
}

// A session pin seeds the region filter, but quota must still be able to
// escape it: otherwise a conversation bound to an account that ran out of
// credits fails every following turn.
func TestSessionPinRegionIsSoftForQuota(t *testing.T) {
	exec := ChatExecutor{Pool: newQuotaFailoverPool(), MaxAttempts: 4}
	loop := exec.newRouteLoop(context.Background(), "acc-global", "", translate.ChatRequest{Model: "MiniMax-M3"})
	if loop.regionFilter != "global" || !loop.softRegion {
		t.Fatalf("session pin must seed a soft region filter: region=%q soft=%v", loop.regionFilter, loop.softRegion)
	}
	loop.excluded["acc-global"] = struct{}{}
	loop.index = 1
	quota := Classified{Kind: KindQuota, Failover: true, Cooldown: time.Hour}
	if !loop.mayFailover(exec, quota, "MiniMax-M3") {
		t.Fatal("a quota refusal must escape a session pin")
	}
	if loop.regionFilter != "" {
		t.Fatalf("session pin region survived a quota refusal: %q", loop.regionFilter)
	}
}
