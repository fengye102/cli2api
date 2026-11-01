package zcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestWeekendClaimOpen pins the Beijing-time window: Friday from 18:00, all
// of Saturday and Sunday, nothing Monday through Friday 17:59. The reference
// implementation gates identically (Date.now()+8h on UTC getters).
func TestWeekendClaimOpen(t *testing.T) {
	bj := func(y int, mo time.Month, d, h, mi int) time.Time {
		return time.Date(y, mo, d, h, mi, 0, 0, time.FixedZone("UTC+8", 8*3600))
	}
	cases := []struct {
		at   time.Time
		want bool
		why  string
	}{
		{bj(2026, 10, 30, 17, 59), false, "Friday 17:59 still closed"},
		{bj(2026, 10, 30, 18, 0), true, "Friday 18:00 opens"},
		{bj(2026, 10, 31, 0, 0), true, "Saturday all day"},
		{bj(2026, 11, 1, 23, 59), true, "Sunday 23:59 still open"},
		{bj(2026, 11, 2, 0, 0), false, "Monday closed"},
		{bj(2026, 11, 4, 12, 0), false, "Wednesday closed"},
	}
	for _, c := range cases {
		if got := WeekendClaimOpen(c.at); got != c.want {
			t.Errorf("WeekendClaimOpen(%s)=%v, want %v (%s)", c.at, got, c.want, c.why)
		}
	}
	// A UTC Friday 10:00 is Beijing Friday 18:00 — must be open.
	utcFri := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	if !WeekendClaimOpen(utcFri) {
		t.Errorf("UTC Friday 10:00 (Beijing 18:00) should be open")
	}
}

// TestWeekendPlanCandidates pins the date-encoded candidate ids: today,
// yesterday and tomorrow in Beijing time, deduplicated, framed as
// zcode-v3-start-plan-<MMDD>-wk. The ±1 day slack covers the midnight
// rollover inside the window edges.
func TestWeekendPlanCandidates(t *testing.T) {
	bj := time.Date(2026, 10, 31, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	got := weekendPlanCandidates(bj)
	want := []string{"zcode-v3-start-plan-1031-wk", "zcode-v3-start-plan-1030-wk", "zcode-v3-start-plan-1101-wk"}
	if len(got) != len(want) {
		t.Fatalf("candidates=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate[%d]=%s, want %s", i, got[i], want[i])
		}
	}
	// Month boundary: Beijing Nov 1 must produce 1031/1101/1102.
	nov1 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	got = weekendPlanCandidates(nov1)
	want = []string{"zcode-v3-start-plan-1101-wk", "zcode-v3-start-plan-1031-wk", "zcode-v3-start-plan-1102-wk"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("month-boundary candidate[%d]=%s, want %s", i, got[i], want[i])
		}
	}
}

// TestIsWeekendPlanID covers the id-shape match plus the display-name match
// the reference implementation needed when upstream renamed ids.
func TestIsWeekendPlanID(t *testing.T) {
	yes := []string{
		"zcode-v3-start-plan-1031-wk",
		"ZCODE-V3-START-PLAN-1101-WK",
		"Weekend Build 2026-10-30",
	}
	no := []string{
		"zcode-v3-start-plan-0817",     // daily Start Plan, not promo
		"zcode-v3-start-plan-wkx",      // not the -wk suffix
		"xcode-v3-start-plan-1031-wk",  // wrong prefix
		"zcode-v3-coding-plan-1031-wk", // paid plan that copied the suffix
	}
	for _, id := range yes {
		if !isWeekendPlanID(id) {
			t.Errorf("isWeekendPlanID(%q)=false, want true", id)
		}
	}
	for _, id := range no {
		if isWeekendPlanID(id) {
			t.Errorf("isWeekendPlanID(%q)=true, want false", id)
		}
	}
}

func weekendTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	return newClaimClient(t, handler, claimOAuthStore())
}

// TestAutoClaimWeekendSkipsActiveWeekend feeds a billing/balance payload with
// an active weekend plan and asserts the sweep reports AlreadyClaimed without
// firing any claim POST.
func TestAutoClaimWeekendSkipsActiveWeekend(t *testing.T) {
	var claims int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/balance"):
			w.Header().Set("Content-Type", "application/json")
			// ends_at far in the future; plan_id carries the -wk promo shape.
			_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[{"plan_id":"zcode-v3-start-plan-1031-wk","name":"Weekend Build","status":"active","ends_at":1893456000}],"balances":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/billing/claim"):
			claims++
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			http.NotFound(w, r)
		}
	})
	c := weekendTestClient(t, handler)

	result, err := c.AutoClaimWeekend(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("AutoClaimWeekend: %v", err)
	}
	if !result.AlreadyClaimed {
		t.Fatalf("want AlreadyClaimed for active weekend plan, got %+v", result)
	}
	if claims != 0 {
		t.Errorf("active plan must not fire claim POSTs, got %d", claims)
	}
}

// TestAutoClaimWeekendTriesCandidatesUntilClaim feeds 1005 for the first
// candidate and code 0 for the second, asserting the sweep moves on and stops
// at the first claimed id.
func TestAutoClaimWeekendTriesCandidatesUntilClaim(t *testing.T) {
	// The candidate ids are date-encoded, so derive them from the same helper the
	// sweep uses instead of pinning a calendar day. Pinning 1031 made this test
	// fail the moment the wall clock rolled past midnight, which is the sort of
	// flake that hides a real regression on the next run.
	candidates := weekendPlanCandidates(time.Now())
	if len(candidates) < 2 {
		t.Fatalf("need at least two candidates, got %v", candidates)
	}
	first, second := candidates[0], candidates[1]
	var claimed []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/balance"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[],"balances":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/billing/claim"):
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				PlanID string `json:"plan_id"`
			}
			_ = json.Unmarshal(raw, &body)
			claimed = append(claimed, body.PlanID)
			if body.PlanID == first {
				// First candidate: 名额未开/已尽 -> keep going.
				_, _ = w.Write([]byte(`{"code":1005,"msg":"名额已用完"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"plan":{"plan_id":"` + body.PlanID + `","name":"Weekend Build"}}}`))
		default:
			http.NotFound(w, r)
		}
	})
	c := weekendTestClient(t, handler)

	result, err := c.AutoClaimWeekend(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("AutoClaimWeekend: %v", err)
	}
	if !result.Claimed {
		t.Fatalf("want Claimed after second candidate, got %+v", result)
	}
	if len(claimed) != 2 {
		t.Fatalf("expected 2 claim attempts (1005 then claim), got %d: %v", len(claimed), claimed)
	}
	if claimed[1] != second {
		t.Fatalf("second attempt = %q want %q (candidates %v)", claimed[1], second, candidates)
	}
}

// TestAutoClaimWeekendEmptyBodyIsNotAClaim covers the refusal shape the live
// sweep hit: a 2xx with an empty body. It unmarshals to code 0, which is also
// the success code, so without an evidence check the sweep would report a claim
// it never got and stop for the whole weekend. The message must also say what
// happened instead of leaving the log line ending in a bare colon.
func TestAutoClaimWeekendEmptyBodyIsNotAClaim(t *testing.T) {
	var claims int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/balance"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[],"balances":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/billing/claim"):
			claims++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	c := weekendTestClient(t, handler)

	result, err := c.AutoClaimWeekend(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("AutoClaimWeekend: %v", err)
	}
	if result.Claimed {
		t.Fatalf("an empty 2xx body must not read as a claim: %+v", result)
	}
	if claims != 1 {
		t.Fatalf("claims=%d want 1 (the round stops on an unlisted code)", claims)
	}
	if strings.TrimSpace(result.Message) == "" {
		t.Fatal("empty message would leave the sweep log line ending in a colon")
	}
	if !strings.Contains(result.Message, "empty upstream body") {
		t.Fatalf("message=%q want it to name the empty body", result.Message)
	}
}

// TestAutoClaimWeekendStopsOnAlreadyClaimed covers 1003 on the first
// candidate: the sweep must report AlreadyClaimed and not try the remaining
// ids.
func TestAutoClaimWeekendStopsOnAlreadyClaimed(t *testing.T) {
	var claims int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/balance"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[],"balances":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/billing/claim"):
			claims++
			_, _ = w.Write([]byte(`{"code":1003,"msg":"already claimed"}`))
		default:
			http.NotFound(w, r)
		}
	})
	c := weekendTestClient(t, handler)

	result, err := c.AutoClaimWeekend(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("AutoClaimWeekend: %v", err)
	}
	if !result.AlreadyClaimed {
		t.Fatalf("want AlreadyClaimed on 1003, got %+v", result)
	}
	if claims != 1 {
		t.Errorf("1003 must stop the sweep, got %d claim attempts", claims)
	}
}

// TestAutoClaimWeekendAllNotOpen covers every candidate answering 1005: the
// sweep must end with neither Claimed nor AlreadyClaimed (caller retries on
// the next tick) and not error.
func TestAutoClaimWeekendAllNotOpen(t *testing.T) {
	var claims int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/balance"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[],"balances":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/billing/claim"):
			claims++
			_, _ = w.Write([]byte(`{"code":1005,"msg":"名额未开放"}`))
		default:
			http.NotFound(w, r)
		}
	})
	c := weekendTestClient(t, handler)

	result, err := c.AutoClaimWeekend(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("AutoClaimWeekend on all-1005 should not hard-error: %v", err)
	}
	if result.Claimed || result.AlreadyClaimed {
		t.Fatalf("all-1005 must not claim, got %+v", result)
	}
	if claims != len(weekendPlanCandidates(time.Now())) {
		t.Errorf("expected %d attempts, got %d", len(weekendPlanCandidates(time.Now())), claims)
	}
}
