package control

import (
	"testing"
	"time"
)

// A refresh that comes back with no models and no error must not replace the
// last known catalog. CatalogSource.Fetch reports (nil, nil) whenever every
// provider failed or no account is configured, and treating that as a fresh
// empty catalog blanked /v1/models while the persisted snapshot on disk was
// still good.
func TestCatalogKeepsLastKnownListWhenRefreshReturnsNothing(t *testing.T) {
	var calls int
	catalog := NewCatalog(func(bool, string, CatalogMode) ([]map[string]any, error) {
		calls++
		if calls == 1 {
			return []map[string]any{{"id": "glm-5.3", "provider": "zcode"}}, nil
		}
		return nil, nil
	})
	catalog.TTL = time.Millisecond

	seed, err := catalog.Get(true, "", CatalogModeMerge)
	if err != nil || len(seed) != 1 {
		t.Fatalf("seed=%v err=%v", seed, err)
	}

	// Let the TTL lapse so the next read refreshes in the background.
	time.Sleep(3 * time.Millisecond)
	if served, _ := catalog.Get(false, "", CatalogModeMerge); len(served) != 1 {
		t.Fatalf("stale read served %v", served)
	}
	// Wait for the background refresh (which returns nothing) to finish.
	time.Sleep(80 * time.Millisecond)

	after, err := catalog.Get(false, "", CatalogModeMerge)
	if err != nil {
		t.Fatalf("an empty refresh must not fail a served catalog: %v", err)
	}
	if len(after) != 1 || after[0]["id"] != "glm-5.3" {
		t.Fatalf("catalog after empty refresh=%v, want the last known list", after)
	}
	if got := catalog.CachedCount("", CatalogModeMerge); got != 1 {
		t.Fatalf("CachedCount=%d want 1", got)
	}
}

// With nothing cached yet, an empty refresh should be remembered as an attempt
// (so reads do not re-poll on every request) without caching a bogus empty
// catalog that a later successful refresh would have to fight past.
func TestCatalogEmptyRefreshWithNoCacheStillReportsNothing(t *testing.T) {
	var calls int
	catalog := NewCatalog(func(bool, string, CatalogMode) ([]map[string]any, error) {
		calls++
		return nil, nil
	})
	catalog.TTL = time.Millisecond

	models, err := catalog.Get(true, "", CatalogModeMerge)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(models) != 0 {
		t.Fatalf("models=%v want none", models)
	}
	before := calls
	if _, _ = catalog.Get(false, "", CatalogModeMerge); calls != before {
		t.Fatalf("a second read re-polled (%d -> %d); the attempt was not recorded", before, calls)
	}
}
