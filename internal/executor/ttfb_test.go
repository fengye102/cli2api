package executor

import (
	"reflect"
	"testing"
	"time"
)

// pickSeq drives the pool through n picks and returns the account IDs in order.
func pickSeq(t *testing.T, pool *Pool, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		item, ok := pool.Pick("", nil)
		if !ok {
			t.Fatalf("pick %d: pool reported no candidate", i)
		}
		ids = append(ids, item.ID)
	}
	return ids
}

func seedTTFB(pool *Pool, id string, samples ...int) {
	for _, ms := range samples {
		pool.ObserveTTFB(id, ms)
	}
}

// A cold pool must route exactly like round-robin: the strategy may only start
// diverging once it has evidence, otherwise enabling it would reshuffle traffic
// on a fresh process for no reason.
func TestLowestTTFBColdPoolMatchesRoundRobin(t *testing.T) {
	urls := []string{"http://a", "http://b", "http://c"}
	ids := []string{"a", "b", "c"}
	ttfbPool := NewPool(urls, ids)
	ttfbPool.SetRoutingStrategy(RoutingStrategyLowestTTFB)
	rrPool := NewPool(urls, ids)

	got := pickSeq(t, ttfbPool, 7)
	want := pickSeq(t, rrPool, 7)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cold lowest-ttfb = %v, round-robin = %v; want identical", got, want)
	}
}

// The point of the strategy: a stalling account stops receiving requests.
func TestLowestTTFBSkipsStalledAccount(t *testing.T) {
	pool := NewPool([]string{"http://a", "http://b", "http://c"}, []string{"a", "b", "c"})
	pool.SetRoutingStrategy(RoutingStrategyLowestTTFB)
	seedTTFB(pool, "a", 300, 310, 290)
	seedTTFB(pool, "b", 280, 270, 300)
	seedTTFB(pool, "c", 9000, 9500, 8800)

	for i, id := range pickSeq(t, pool, 12) {
		if id == "c" {
			t.Fatalf("pick %d went to the stalled account %q; the band must exclude it", i, id)
		}
	}
}

// Two accounts that answer alike keep sharing the route. The strategy narrows
// to a band, it does not crown a single winner.
func TestLowestTTFBSpreadsAcrossEquallyFastAccounts(t *testing.T) {
	pool := NewPool([]string{"http://a", "http://b"}, []string{"a", "b"})
	pool.SetRoutingStrategy(RoutingStrategyLowestTTFB)
	seedTTFB(pool, "a", 300, 320, 310)
	seedTTFB(pool, "b", 310, 300, 330)

	counts := map[string]int{}
	for _, id := range pickSeq(t, pool, 8) {
		counts[id]++
	}
	if counts["a"] < 3 || counts["b"] < 3 {
		t.Fatalf("band rotation = %v; two equally fast accounts must both stay in use", counts)
	}
}

// A fast sample is evidence with a shelf life. Once it expires the account is
// scored on the pool prior again, so a temporarily slow account is not banned
// for the life of the process.
func TestLowestTTFBForgivesExpiredSample(t *testing.T) {
	pool := NewPool([]string{"http://a", "http://b"}, []string{"a", "b"})
	pool.SetRoutingStrategy(RoutingStrategyLowestTTFB)
	seedTTFB(pool, "a", 300, 300, 300)
	pool.mu.Lock()
	pool.ttfb["b"] = ttfbSample{ms: 9000, at: time.Now().Add(-ttfbTTL - time.Minute)}
	pool.mu.Unlock()

	if _, fresh := pool.TTFB("b"); fresh {
		t.Fatal("TTFB reported a stale sample as fresh")
	}
	counts := map[string]int{}
	for _, id := range pickSeq(t, pool, 6) {
		counts[id]++
	}
	if counts["b"] == 0 {
		t.Fatalf("expired sample still excluded b (counts=%v); it must be scored on the prior", counts)
	}
}

// Readings that cannot be a real first byte must not enter the window: an empty
// or locally-failed attempt would otherwise look like the fastest account alive.
func TestObserveTTFBRejectsNoise(t *testing.T) {
	pool := NewPool([]string{"http://a"}, []string{"a"})
	pool.ObserveTTFB("a", 0)
	pool.ObserveTTFB("a", ttfbFloorMs-1)
	pool.ObserveTTFB("", 300)
	if _, ok := pool.TTFB("a"); ok {
		t.Fatal("TTFB recorded a sub-floor reading")
	}
}

// The window is a smoothed estimate, not the last reading: one slow request on
// a healthy account must not be enough to exile it.
func TestObserveTTFBSmoothsSingleOutlier(t *testing.T) {
	pool := NewPool([]string{"http://a"}, []string{"a"})
	seedTTFB(pool, "a", 300, 300, 300)
	pool.ObserveTTFB("a", 9000)
	got, ok := pool.TTFB("a")
	if !ok {
		t.Fatal("TTFB lost the sample after one outlier")
	}
	if got >= 1000 {
		t.Fatalf("EWMA = %dms after one 9s outlier on a 300ms account; want the outlier winsorized below 1s", got)
	}
	// A sustained slowdown must still climb: the same 9s reading repeated
	// cannot be dismissed as a hiccup forever.
	for i := 0; i < 8; i++ {
		pool.ObserveTTFB("a", 9000)
	}
	if slow, _ := pool.TTFB("a"); slow < 3000 {
		t.Fatalf("EWMA = %dms after nine 9s readings; sustained slowness must demote the account", slow)
	}
}

// Switching strategy must not throw away what the pool learned about the
// accounts' latency.
func TestSetRoutingStrategyKeepsTTFBSamples(t *testing.T) {
	pool := NewPool([]string{"http://a"}, []string{"a"})
	pool.SetRoutingStrategy(RoutingStrategyLowestTTFB)
	seedTTFB(pool, "a", 250, 260, 240)
	pool.SetRoutingStrategy(RoutingStrategyRoundRobin)
	pool.SetRoutingStrategy(RoutingStrategyLowestTTFB)
	if _, ok := pool.TTFB("a"); !ok {
		t.Fatal("strategy switch dropped the latency window")
	}
}

// The settings layer must accept the new strategy and keep rejecting junk.
func TestNormalizeRoutingStrategyLowestTTFB(t *testing.T) {
	for _, in := range []string{"lowest-ttfb", "LOWEST-TTFB", "  lowest-ttfb  "} {
		if got := NormalizeRoutingStrategy(in); got != RoutingStrategyLowestTTFB {
			t.Fatalf("NormalizeRoutingStrategy(%q) = %q", in, got)
		}
	}
	if got := NormalizeRoutingStrategy("fastest"); got != RoutingStrategyRoundRobin {
		t.Fatalf("unknown strategy normalized to %q, want round-robin", got)
	}
}
