package raccoon

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// onboardingClient wires a Client to a stub main site and answers the two grant
// entries plus the bills ledger, so a check-in can run end to end.
func onboardingClient(t *testing.T, grants map[string]string, ledger string) (*Client, *[]string) {
	t.Helper()
	var seenPaths []string
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, desktopGrantPath), strings.HasSuffix(r.URL.Path, mobileGrantPath):
			seenPaths = append(seenPaths, r.URL.Path)
			if r.Method != http.MethodPost {
				t.Errorf("%s method = %s, want POST", r.URL.Path, r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
				t.Errorf("%s Authorization = %q", r.URL.Path, got)
			}
			body, ok := grants[r.URL.Path]
			if !ok {
				body = `{"code":0,"data":{"granted":false}}`
			}
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, settingInfoPath):
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, billsPath):
			_, _ = w.Write([]byte(ledger))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return client, &seenPaths
}

// TestClaimLoginGrantClaimsDesktopReward is the happy path: upstream says
// granted=true and the points come from popup, not from our nominal 3000.
func TestClaimLoginGrantClaimsDesktopReward(t *testing.T) {
	client, _ := onboardingClient(t, map[string]string{
		desktopGrantPath: `{"code":0,"data":{"granted":true,"popup":{"source":"desktop_login","points":3200,"expire_at":"2026-12-01"}}}`,
	}, billsBody())

	result, err := client.Claim(context.Background(), "acc-1", TaskDesktopLoginGrant)
	if err != nil {
		t.Fatalf("Claim error: %v", err)
	}
	if !result.Claimed || result.PlanID != TaskDesktopLoginGrant {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Message, "3200") {
		t.Errorf("message should name the real points, got %q", result.Message)
	}
}

// TestClaimLoginGrantMobileUsesAppIdentityHeaders pins the phone branch's
// identity: the upstream settles that reward against the app, so the platform
// and version must say so or the reward silently does not qualify.
func TestClaimLoginGrantMobileUsesAppIdentityHeaders(t *testing.T) {
	var sawPlatform, sawVersion string
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, mobileGrantPath) {
			sawPlatform = r.Header.Get("X-Client-Platform")
			sawVersion = r.Header.Get("X-Client-Version")
			_, _ = w.Write([]byte(`{"code":0,"data":{"granted":true}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	result, err := client.Claim(context.Background(), "acc-1", TaskMobileLoginGrant)
	if err != nil {
		t.Fatalf("Claim error: %v", err)
	}
	if sawPlatform != "app-android" || sawVersion != "v1.0.3" {
		t.Errorf("app identity headers = %q/%q", sawPlatform, sawVersion)
	}
	// No popup in the answer: fall back to the advertised value rather than
	// reporting a reward of zero points.
	if !result.Claimed || !strings.Contains(result.Message, "3000") {
		t.Errorf("result = %+v, want the nominal 3000 in the message", result)
	}
}

// TestClaimLoginGrantAlreadyPaidOut: granted=false is not an error — it means
// there is nothing left on this branch, and it must not be reported as claimed.
func TestClaimLoginGrantAlreadyPaidOut(t *testing.T) {
	client, _ := onboardingClient(t, map[string]string{
		desktopGrantPath: `{"code":0,"data":{"granted":false}}`,
	}, billsBody())

	result, err := client.Claim(context.Background(), "acc-1", TaskDesktopLoginGrant)
	if err != nil {
		t.Fatalf("Claim error: %v", err)
	}
	if result.Claimed || !result.AlreadyClaimed {
		t.Fatalf("result = %+v, want already-claimed", result)
	}
}

// TestClaimLoginGrantIsMemoized keeps the daily hook from re-posting a settled
// branch every morning for the life of the process.
func TestClaimLoginGrantIsMemoized(t *testing.T) {
	client, seen := onboardingClient(t, map[string]string{
		desktopGrantPath: `{"code":0,"data":{"granted":false}}`,
	}, billsBody())

	if _, err := client.Claim(context.Background(), "acc-1", TaskDesktopLoginGrant); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	second, err := client.Claim(context.Background(), "acc-1", TaskDesktopLoginGrant)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if !second.AlreadyClaimed {
		t.Fatalf("second Claim = %+v, want already-claimed from the memo", second)
	}
	if len(*seen) != 1 {
		t.Fatalf("upstream calls = %d (%v), want 1 — the memo must stop the repeat", len(*seen), *seen)
	}

	plans, err := client.PreviewClaim(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("PreviewClaim: %v", err)
	}
	for _, plan := range plans {
		if plan.PlanID == TaskDesktopLoginGrant && plan.Status != "claimed" {
			t.Errorf("desktop task status = %q, want claimed once settled", plan.Status)
		}
	}
}

// TestClaimRejectsAnUnknownTask: the console hands back whatever plan_id it was
// given, so a stale id must fail loudly instead of posting to a default branch.
func TestClaimRejectsAnUnknownTask(t *testing.T) {
	client, seen := onboardingClient(t, nil, billsBody())
	if _, err := client.Claim(context.Background(), "acc-1", "zcode-v3-start-plan"); err == nil {
		t.Fatal("an unknown task must error")
	}
	if len(*seen) != 0 {
		t.Fatalf("no grant call should have been made, got %v", *seen)
	}
}

// TestPreviewClaimListsBothBranches keeps the two rewards discoverable; the
// status is deliberately unknown because upstream has no read endpoint.
func TestPreviewClaimListsBothBranches(t *testing.T) {
	client, _ := onboardingClient(t, nil, billsBody())
	plans, err := client.PreviewClaim(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("PreviewClaim: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %+v, want 2", plans)
	}
	if plans[0].PlanID != TaskDesktopLoginGrant || plans[1].PlanID != TaskMobileLoginGrant {
		t.Errorf("order = %q,%q", plans[0].PlanID, plans[1].PlanID)
	}
	for _, plan := range plans {
		if plan.Status != "unknown" {
			t.Errorf("%s status = %q, want unknown before a claim", plan.PlanID, plan.Status)
		}
	}
}

// TestClaimLoginGrantsIsBestEffort: one branch failing must not swallow the
// other branch's reward, because this runs inside the check-in path.
func TestClaimLoginGrantsIsBestEffort(t *testing.T) {
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, desktopGrantPath):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"message":"boom"}`))
		case strings.HasSuffix(r.URL.Path, mobileGrantPath):
			_, _ = w.Write([]byte(`{"code":0,"data":{"granted":true,"popup":{"points":3000}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	points, notes := client.ClaimLoginGrants(context.Background(), "acc-1")
	if points != 3000 {
		t.Fatalf("points = %d, want 3000 from the branch that answered", points)
	}
	if len(notes) != 2 {
		t.Fatalf("notes = %v, want one per branch", notes)
	}
}

// TestCheckinClaimsFirstLoginRewards is the wiring proof: a successful daily
// check-in also takes the one-time rewards and says so in its message.
func TestCheckinClaimsFirstLoginRewards(t *testing.T) {
	client, seen := onboardingClient(t, map[string]string{
		mobileGrantPath: `{"code":0,"data":{"granted":true,"popup":{"points":3000}}}`,
	}, billsBody([2]string{"100", todayAt("09:30")}))

	result, err := client.Checkin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Checkin error: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q", result.Status)
	}
	if !strings.Contains(result.Message, "首登奖励") {
		t.Errorf("message should mention the first-login reward, got %q", result.Message)
	}
	if len(*seen) != 2 {
		t.Errorf("grant calls = %v, want both branches tried", *seen)
	}
}

// TestCheckinSurvivesAFailingRewardEndpoint: a reward endpoint in trouble must
// leave a check-in that already landed its daily credit green.
func TestCheckinSurvivesAFailingRewardEndpoint(t *testing.T) {
	var seenPaths []string
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, settingInfoPath):
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, billsPath):
			_, _ = w.Write([]byte(billsBody([2]string{"100", todayAt("09:30")})))
		case strings.HasSuffix(r.URL.Path, desktopGrantPath), strings.HasSuffix(r.URL.Path, mobileGrantPath):
			seenPaths = append(seenPaths, r.URL.Path)
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	result, err := client.Checkin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("a reward outage must not fail the check-in: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if strings.Contains(result.Message, "首登奖励") {
		t.Errorf("message claims a reward that did not land: %q", result.Message)
	}
	if len(seenPaths) != 2 {
		t.Errorf("grant attempts = %v, want both tried", seenPaths)
	}
}

// TestRaccoonExposesTheClaimCapability keeps the console's claim routes wired:
// a provider whose Adapter.Claim is nil answers "claim is not available".
func TestRaccoonExposesTheClaimCapability(t *testing.T) {
	client, _ := onboardingClient(t, nil, billsBody())
	var claimer providers.AccountClaimer = client
	if _, ok := claimer.(interface {
		PreviewClaim(context.Context, string) ([]providers.ClaimPlan, error)
	}); !ok {
		t.Fatal("raccoon must satisfy providers.AccountClaimer")
	}
	if adapter := client.Adapter(); adapter.Claim == nil {
		t.Fatal("Adapter.Claim must be wired or the claim endpoints stay off")
	}
}
