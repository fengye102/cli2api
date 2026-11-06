package minimax

import (
	"testing"
	"time"
)

// realShape is the response the gateway actually returns: the first pack quotes
// its amounts as strings and omits consumed_amount, the second sends bare
// numbers. Both must add up.
const realShape = `{"details":[
 {"credit_type":2,"granted_amount":"400.00","remaining_amount":"400.00","consumed_amount":"0.00","granted_at_ms":1793985922485,"expire_at_ms":1796572800000},
 {"consumed_amount":"0.53","granted_at_ms":1793976677805,"expire_at_ms":1796486400000,"credit_type":2,"granted_amount":"400.00","remaining_amount":"399.47"}
],"total_count":2,"base_resp":{"status_code":0,"status_msg":"success"}}`

func TestParseCreditDetailsSumsPacks(t *testing.T) {
	now := time.Date(2026, time.November, 6, 20, 0, 0, 0, time.UTC)
	info, err := parseCreditDetails([]byte(realShape), now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.Total != 800 || info.Remaining != 799.47 || info.Used != 0.53 {
		t.Fatalf("totals wrong: total=%v remain=%v used=%v", info.Total, info.Remaining, info.Used)
	}
	if info.Exceeded {
		t.Fatal("799 credits left must not be reported as exhausted")
	}
	if len(info.Packages) != 2 {
		t.Fatalf("want 2 packs, got %d", len(info.Packages))
	}
	// Sorted by expiry: the pack expiring 1796486400 comes first.
	if info.Packages[0].EndsAt != 1796486400 || info.Packages[0].Remain != 399.47 {
		t.Fatalf("packs not sorted by expiry: %+v", info.Packages)
	}
	if info.ExpiresAt != 1796486400 || info.ExpiringRemain != 399.47 {
		t.Fatalf("soonest expiry wrong: at=%d remain=%v", info.ExpiresAt, info.ExpiringRemain)
	}
	if len(info.Windows) != 1 || info.Windows[0].Unit != creditUnit {
		t.Fatalf("window wrong: %+v", info.Windows)
	}
	if info.Windows[0].ResetAt != time.Unix(1796486400, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("reset at not the soonest expiry: %q", info.Windows[0].ResetAt)
	}
	if info.Windows[0].Percentage <= 0 || info.Windows[0].Percentage >= 1 {
		t.Fatalf("percentage should be 0.53/800, got %v", info.Windows[0].Percentage)
	}
	if info.ProviderID != ProviderID || info.FetchedAt == "" {
		t.Fatalf("provider stamp missing: %+v", info)
	}
}

// An account with no packs is the international account today: zero credits,
// which must read as exhausted so routing skips it before the next 1008.
func TestParseCreditDetailsEmptyIsExhausted(t *testing.T) {
	info, err := parseCreditDetails([]byte(`{"details":[],"total_count":0,"base_resp":{"status_code":0,"status_msg":"success"}}`), time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !info.Exceeded || info.Remaining != 0 || info.Total != 0 {
		t.Fatalf("empty pack list must be exhausted: %+v", info)
	}
	if info.ExpiresAt != 0 || info.ExpiringRemain != 0 {
		t.Fatalf("no expiry expected: %+v", info)
	}
}

// A pack the gateway forgot to bill shows the derived usage instead of a pack
// that is 100% remaining and 53% consumed at the same time.
func TestParseCreditDetailsDerivesMissingConsumed(t *testing.T) {
	body := `{"details":[{"granted_amount":"400.00","remaining_amount":"368.00","expire_at_ms":1796572800000}],"base_resp":{"status_code":0,"status_msg":"success"}}`
	info, err := parseCreditDetails([]byte(body), time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.Used != 32 || info.Remaining != 368 || info.Total != 400 {
		t.Fatalf("derived usage wrong: %+v", info)
	}
}

func TestParseCreditDetailsRejectsUpstreamError(t *testing.T) {
	if _, err := parseCreditDetails([]byte(`{"base_resp":{"status_code":1,"status_msg":"invalid token"}}`), time.Now()); err == nil {
		t.Fatal("an upstream error must not be read as an empty balance")
	}
}
