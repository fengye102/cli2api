package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestParseBannerPlanIDs covers the feed shapes: the claim action sits wherever
// the banner keeps it, each plan id counts once, and an unreadable feed is an
// error rather than an empty list.
func TestParseBannerPlanIDs(t *testing.T) {
	nested := `{"code":0,"data":{"deliveries":[
		{"banner":{"background":{"args":{"zcode_plan":{"name":"ZCODE WEEKEND BUILD"}}},
		 "buttons":[{"text":"Claim","action":{"type":"claim_zcode_plan","args":{"plan_id":" zcode-gift-wk-1030 "}}}]}},
		{"banner":{"buttons":[{"action":{"type":"open_url","args":{"url":"https://z.ai"}}},
			{"action":{"type":"claim_zcode_plan","args":{"plan_id":"zcode-gift-wk-1031"}}},
			{"action":{"type":"claim_zcode_plan","args":{"plan_id":"zcode-gift-wk-1030"}}}]}}
	]}}`
	empty := `{"code":0,"msg":"","data":{"deliveries":[]}}`

	ids, err := parseBannerPlanIDs([]byte(nested))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"zcode-gift-wk-1030", "zcode-gift-wk-1031"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids=%v want %v (trimmed, deduped, feed order)", ids, want)
	}

	ids, err = parseBannerPlanIDs([]byte(empty))
	if err != nil || len(ids) != 0 {
		t.Fatalf("an empty feed lists nothing: ids=%v err=%v", ids, err)
	}

	for _, bad := range []string{"", "not json", `{"code":3001,"msg":"parameter error","data":{"deliveries":[]}}`, `{"data":`} {
		if _, err := parseBannerPlanIDs([]byte(bad)); err == nil {
			t.Fatalf("body %q must not read as an empty feed", bad)
		}
	}
}

// TestAutoClaimWeekendClaimsTheBannerPlan is the #503 case: the plan the app is
// advertising has an id the date scheme cannot derive, and it is in no billing
// list while the banner is up. The feed names it, so the sweep must claim it.
func TestAutoClaimWeekendClaimsTheBannerPlan(t *testing.T) {
	const bannerID = "zcode-gift-weekend-build-1030"
	var claimed []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/marketing/touch"):
			if got := r.Header.Get("X-Client-Language"); got == "" {
				t.Errorf("feed must carry X-Client-Language")
			}
			if !strings.Contains(r.URL.RawQuery, "seq=") {
				t.Errorf("feed must carry seq: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"deliveries":[{"banner":{"buttons":[{"action":{"type":"claim_zcode_plan","args":{"plan_id":"` + bannerID + `"}}}]}}]}}`))
		case strings.HasSuffix(r.URL.Path, "/billing/balance"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[],"balances":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/billing/claim"):
			var body struct {
				PlanID string `json:"plan_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			claimed = append(claimed, body.PlanID)
			if body.PlanID != bannerID {
				_, _ = w.Write([]byte(`{"code":1001,"msg":"plan not found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"Plan claimed successfully.","data":{"plan":{"plan_id":"` + bannerID + `","name":"ZCODE WEEKEND BUILD"}}}`))
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
		t.Fatalf("the banner plan must be claimable, got %+v", result)
	}
	if len(claimed) != 1 || claimed[0] != bannerID {
		t.Fatalf("claimed=%v want [%s] first", claimed, bannerID)
	}
}

// TestAutoClaimWeekendFallsBackWhenTheFeedDoesNotRead keeps the date guesses in
// play for the rounds where the feed is down, and says so in the round's message.
func TestAutoClaimWeekendFallsBackWhenTheFeedDoesNotRead(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/marketing/touch"):
			http.Error(w, "upstream is having a day", http.StatusBadGateway)
		case strings.HasSuffix(r.URL.Path, "/billing/balance"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[],"balances":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/billing/claim"):
			_, _ = w.Write([]byte(`{"code":1005,"msg":"名额已尽"}`))
		default:
			http.NotFound(w, r)
		}
	})
	c := weekendTestClient(t, handler)

	result, err := c.AutoClaimWeekend(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("AutoClaimWeekend: %v", err)
	}
	if result.Claimed || result.AlreadyClaimed {
		t.Fatalf("nothing was claimed: %+v", result)
	}
	if !strings.Contains(result.Message, "banner feed unread") {
		t.Fatalf("message=%q must name the unread feed", result.Message)
	}
	if !strings.Contains(result.Message, "last code 1005") {
		t.Fatalf("message=%q must keep the date-candidate outcome", result.Message)
	}
}

// TestWeekendClaimWindowOpen pins the window the sweep now respects: Friday
// 18:00 Beijing through Sunday midnight, nothing the rest of the week.
func TestWeekendClaimWindowOpen(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"friday before 18:00", time.Date(2026, 10, 30, 17, 59, 0, 0, beijingLocation()), false},
		{"friday 18:00", time.Date(2026, 10, 30, 18, 0, 0, 0, beijingLocation()), true},
		{"saturday noon", time.Date(2026, 10, 31, 12, 0, 0, 0, beijingLocation()), true},
		{"sunday last minute", time.Date(2026, 11, 1, 23, 59, 0, 0, beijingLocation()), true},
		{"monday midnight", time.Date(2026, 11, 2, 0, 0, 0, 0, beijingLocation()), false},
		{"tuesday", time.Date(2026, 11, 3, 12, 0, 0, 0, beijingLocation()), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WeekendClaimOpen(tc.at); got != tc.want {
				t.Fatalf("WeekendClaimOpen(%s)=%v want %v", tc.at, got, tc.want)
			}
			// The adapter exposes the same answer to the maintenance sweep.
			if got := NewClient(nil).WeekendClaimWindowOpen(tc.at); got != tc.want {
				t.Fatalf("WeekendClaimWindowOpen(%s)=%v want %v", tc.at, got, tc.want)
			}
		})
	}
}
