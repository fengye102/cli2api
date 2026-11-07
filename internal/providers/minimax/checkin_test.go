package minimax

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// signinStub is a fake agent gateway: it serves the sign-in panel, the claim,
// and the credit packs, and records what the adapter sent.
type signinStub struct {
	mu          sync.Mutex
	remaining   float64
	statusBody  string
	claimBody   string
	claimCalls  int
	seenQuery   string
	seenAuth    string
	seenMethod  string
	seenBody    string
	creditCalls int
}

func (s *signinStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case CreditDetailsPath:
			s.creditCalls++
			io.WriteString(w, `{"base_resp":{"status_code":0,"status_msg":"ok"},"details":[{"granted_at_ms":1,"expire_at_ms":1796572800000,"credit_type":2,"granted_amount":"400.00","remaining_amount":"`+trimFloat(s.remaining)+`","consumed_amount":"0.00"}],"total_count":1}`)
			return
		case SigninStatusPath:
			s.seenQuery = r.URL.RawQuery
			s.seenAuth = r.Header.Get("Authorization")
			io.WriteString(w, s.statusBody)
			return
		case SigninClaimPath:
			s.claimCalls++
			s.seenMethod = r.Method
			s.seenQuery = r.URL.RawQuery
			raw, _ := io.ReadAll(r.Body)
			s.seenBody = string(raw)
			io.WriteString(w, s.claimBody)
			if strings.Contains(s.claimBody, `"claim_result":1`) {
				s.remaining += 400
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

const (
	statusClaimable = `{"base_resp":{"status_code":0,"status_msg":"ok"},"data":{"scene":2,"days":[{"day_no":1,"points":400,"bonus_points":0,"status":2,"is_today":true},{"day_no":2,"points":400,"status":1,"is_today":false}]}}`
	statusClaimed   = `{"base_resp":{"status_code":0,"status_msg":"ok"},"data":{"scene":2,"days":[{"day_no":1,"points":400,"status":3,"is_today":true}]}}`
	statusNotYet    = `{"base_resp":{"status_code":0,"status_msg":"ok"},"data":{"scene":2,"days":[{"day_no":1,"points":400,"status":1,"is_today":true}]}}`
	statusBadZone   = `{"base_resp":{"status_code":1406010011,"status_msg":"invalid timezone_id"}}`
	claimOK         = `{"base_resp":{"status_code":0,"status_msg":"ok"},"data":{"claim_result":1,"day_no":1,"points":400,"expire_at_ms":1796572800000,"claim_id":"123"}}`
	claimAlready    = `{"base_resp":{"status_code":0,"status_msg":"ok"},"data":{"claim_result":2,"day_no":1,"points":400,"expire_at_ms":1796572800000}}`
)

func newCheckinClient(t *testing.T, stub *signinStub) (*Client, context.Context) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	store := storeWith(t, Credential{AccessToken: "at", RefreshToken: "rt", Region: "cn", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	client := NewClient(store)
	client.siteResolver = fixedSite(srv.URL)
	return client, context.Background()
}

func TestCheckinClaimsTodaysReward(t *testing.T) {
	stub := &signinStub{statusBody: statusClaimable, claimBody: claimOK}
	client, ctx := newCheckinClient(t, stub)

	result, err := client.Checkin(ctx, "acc_minimax")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q (%s)", result.Status, result.Message)
	}
	if result.RewardCredits != 400 {
		t.Fatalf("reward = %v, want the real 400 delta", result.RewardCredits)
	}
	// The gateway rejects a missing timezone_id, so it must be on the query.
	if !strings.Contains(stub.seenQuery, "timezone_id=") {
		t.Fatalf("claim query = %q, want a timezone_id", stub.seenQuery)
	}
	if stub.seenAuth != "Bearer at" {
		t.Fatalf("authorization = %q", stub.seenAuth)
	}
	if stub.seenMethod != http.MethodPost || stub.seenBody != "{}" {
		t.Fatalf("claim = %s %q, want POST {}", stub.seenMethod, stub.seenBody)
	}
	for _, want := range []string{"第 1 天", "+400", "0 → 400", "2026-", "到期"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q missing %q", result.Message, want)
		}
	}
}

func TestCheckinReportsAlreadyClaimedFromThePanel(t *testing.T) {
	stub := &signinStub{statusBody: statusClaimed, claimBody: claimOK}
	client, ctx := newCheckinClient(t, stub)

	result, err := client.Checkin(ctx, "acc_minimax")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.Status != "already" {
		t.Fatalf("status = %q", result.Status)
	}
	if stub.claimCalls != 0 {
		t.Fatalf("claimed %d times, want none: the panel already said claimed", stub.claimCalls)
	}
}

func TestCheckinTreatsClaimResultAlreadyClaimedAsDone(t *testing.T) {
	// The panel can lag the claim: status says claimable, the claim answers 2.
	stub := &signinStub{statusBody: statusClaimable, claimBody: claimAlready}
	client, ctx := newCheckinClient(t, stub)

	result, err := client.Checkin(ctx, "acc_minimax")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.Status != "already" {
		t.Fatalf("status = %q, want already", result.Status)
	}
}

func TestCheckinSkipsADayItCannotClaim(t *testing.T) {
	stub := &signinStub{statusBody: statusNotYet, claimBody: claimOK}
	client, ctx := newCheckinClient(t, stub)

	result, err := client.Checkin(ctx, "acc_minimax")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.Status != "skipped" || !result.Valid() {
		t.Fatalf("result = %+v", result)
	}
	if stub.claimCalls != 0 {
		t.Fatalf("claim ran on a day it cannot claim")
	}
}

func TestCheckinSurfacesTheTimezoneRejection(t *testing.T) {
	stub := &signinStub{statusBody: statusBadZone, claimBody: claimOK}
	client, ctx := newCheckinClient(t, stub)

	_, err := client.Checkin(ctx, "acc_minimax")
	if err == nil {
		t.Fatal("a 1406010011 answer must be an error, not a silent skip")
	}
	if !strings.Contains(err.Error(), "invalid timezone_id") {
		t.Fatalf("error = %v", err)
	}
}

func TestCheckinTimezoneHonoursTheOverride(t *testing.T) {
	t.Setenv(checkinTimezoneEnv, "America/New_York")
	if got := checkinTimezone("cn"); got != "America/New_York" {
		t.Fatalf("timezone = %q", got)
	}
	t.Setenv(checkinTimezoneEnv, "Not/AZone")
	if got := checkinTimezone("cn"); got != defaultCheckinTimezone {
		t.Fatalf("an invalid override must fall back, got %q", got)
	}
}

func TestMinimaxDeclaresTheCheckinPolicy(t *testing.T) {
	policy, ok := providers.CheckinFor(ProviderID, "cn")
	if !ok || policy == nil {
		t.Fatal("minimax must declare a check-in policy so the scheduler runs it")
	}
	if _, ok := providers.CheckinFor(ProviderID, "global"); !ok {
		t.Fatal("the international region needs the policy too")
	}
}

func TestCheckinMessageFallsBackWhenTheBalanceCannotBeRead(t *testing.T) {
	stub := &signinStub{statusBody: statusClaimable, claimBody: claimOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CreditDetailsPath {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		stub.handler()(w, r)
	}))
	defer srv.Close()
	store := storeWith(t, Credential{AccessToken: "at", RefreshToken: "rt", Region: "cn", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	client := NewClient(store)
	client.siteResolver = fixedSite(srv.URL)

	result, err := client.Checkin(context.Background(), "acc_minimax")
	if err != nil {
		t.Fatalf("a broken balance read must not fail the claim: %v", err)
	}
	if result.Status != "success" || result.RewardCredits != 400 {
		t.Fatalf("result = %+v", result)
	}
	if strings.Contains(result.Message, "→") {
		t.Fatalf("message claims a delta it never measured: %q", result.Message)
	}
}

// unused but kept honest: the stub's JSON must stay parseable by the real types.
func TestSigninPanelParsesTheRealShape(t *testing.T) {
	var panel signinPanel
	if err := json.Unmarshal([]byte(statusClaimable), &panel); err != nil {
		t.Fatalf("panel: %v", err)
	}
	today, ok := panel.today()
	if !ok || today.DayNo.float() != 1 || today.Status.float() != signinDayClaimable {
		t.Fatalf("today = %+v", today)
	}
}
