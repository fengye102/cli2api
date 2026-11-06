package loomy

import (
	"context"
	"strings"
	"testing"
)

// The task table's points are a promise, not a receipt: the complete call can
// succeed while the balance never moves. The claim message must show the
// balance it actually read, or an operator cannot tell "cleared 8 tasks, gained
// 10000" from "cleared 8 tasks, gained nothing".
func TestClaimAllReportsCreditsThatActuallyLanded(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	// A gateway that marks tasks complete but credits nothing: the operator's
	// exact complaint shape.
	gateway.creditOnClaim = false

	result, err := client.Claim(context.Background(), "acc-1", claimAllPlanID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !strings.Contains(result.Message, "任务标称") {
		t.Fatalf("message = %q, want it to mark the sum as advertised", result.Message)
	}
	if !strings.Contains(result.Message, "（+0）") {
		t.Fatalf("message = %q, want a +0 confirmed delta when nothing landed", result.Message)
	}
}

// When the credits do land, the delta is reported as the real number.
func TestClaimAllReportsConfirmedDelta(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	result, err := client.Claim(context.Background(), "acc-1", claimAllPlanID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !strings.Contains(result.Message, "（+10000）") {
		t.Fatalf("message = %q, want the confirmed +10000 delta", result.Message)
	}
}
