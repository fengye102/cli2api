package runtime

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// TestRetryableCheckinFailureStopsOnHumanVerification is the core of gap 3: an
// answer that wants an interactive captcha cannot be cleared by waiting, so the
// ladder must not spend five more attempts pretending otherwise.
func TestRetryableCheckinFailureStopsOnHumanVerification(t *testing.T) {
	for _, message := range []string{
		"签到失败：需要人机验证",
		"zcode claim captcha failed: captcha verify failed: 需人工验证",
		"风控拦截，请手动验证",
	} {
		if retryableCheckinFailure(message) {
			t.Errorf("retryableCheckinFailure(%q) = true, want false", message)
		}
	}
}

// TestRetryableCheckinFailureStillRetriesTransientTransients guards the other
// direction: the busy/pending answers the ladder exists for must keep retrying.
func TestRetryableCheckinFailureStillRetriesTransientTransients(t *testing.T) {
	for _, message := range []string{
		"当前参与用户太多，请稍后再试（9074）",
		"今日未见积分入账（发放触发未生效，稍后重试）",
	} {
		if !retryableCheckinFailure(message) {
			t.Errorf("retryableCheckinFailure(%q) = false, want true", message)
		}
	}
}

// TestCheckinDueStopsAfterHumanVerification walks the scheduler, not just the
// predicate: once the recorded message asks for a human, no later rung fires —
// while the same account with a busy answer does get its 15-minute retry.
func TestCheckinDueStopsAfterHumanVerification(t *testing.T) {
	day := time.Date(2026, 11, 6, 0, 0, 0, 0, time.Local)
	due, err := checkinSlot("09:00", "acc-human", day)
	if err != nil {
		t.Fatal(err)
	}
	// First attempt one minute after the slot, i.e. the ladder's rung 1 is the
	// next thing that would fire.
	last := due.Add(time.Minute).Format(time.RFC3339Nano)
	account := Account{
		ID:                "acc-human",
		Enabled:           true,
		AutoCheckin:       true,
		LastCheckinAt:     last,
		LastCheckinStatus: "error",
		LastCheckinMsg:    "签到失败：需要人机验证",
	}
	if checkinDueFor(account, "09:00", due.Add(20*time.Minute), false, 1) {
		t.Fatal("a human-verification failure must not be retried 15 minutes later")
	}

	// Same shape, transient reason: the ladder must still fire.
	transient := account
	transient.ID = "acc-busy"
	transient.LastCheckinMsg = "当前参与用户太多，请稍后再试（9074）"
	if !checkinDueFor(transient, "09:00", due.Add(20*time.Minute), false, 1) {
		t.Fatal("a busy answer must still reach the ladder's first rung")
	}
}

// needsHumanClaimer answers every round with the captcha failure zcode tags.
type needsHumanClaimer struct {
	calls atomic.Int64
	open  bool
}

func (n *needsHumanClaimer) AutoClaimWeekend(context.Context, string) (providers.ClaimResult, error) {
	n.calls.Add(1)
	return providers.ClaimResult{}, fmt.Errorf("zcode claim captcha failed: captcha required: %w",
		providers.ErrNeedsHumanVerification)
}

func (n *needsHumanClaimer) WeekendClaimWindowOpen(time.Time) bool { return n.open }

func (n *needsHumanClaimer) PreviewClaim(context.Context, string) ([]providers.ClaimPlan, error) {
	return nil, nil
}

func (n *needsHumanClaimer) Claim(context.Context, string, string) (providers.ClaimResult, error) {
	return providers.ClaimResult{}, nil
}

// TestWeekendSweepStopsOnHumanVerification: one round, then silence. Without
// this the sweep re-posted a captcha-doomed claim every ten minutes for the
// whole weekend.
func TestWeekendSweepStopsOnHumanVerification(t *testing.T) {
	weekendNeedsHuman = map[string]string{}
	weekendClaimedAt = map[string]time.Time{}
	claimer := &needsHumanClaimer{open: true}
	manager, account := newWeekendSweepManager(t, claimer)
	saturday := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	manager.runWeekendClaims(context.Background(), saturday)
	if claimer.calls.Load() != 1 {
		t.Fatalf("rounds=%d, want 1", claimer.calls.Load())
	}
	if _, flagged := weekendNeedsHuman[account.ID]; !flagged {
		t.Fatal("the account should be flagged as needing a human")
	}

	// Well past the ten-minute throttle: still silent, because a person has to
	// step in first.
	manager.runWeekendClaims(context.Background(), saturday.Add(30*time.Minute))
	manager.runWeekendClaims(context.Background(), saturday.Add(3*time.Hour))
	if claimer.calls.Load() != 1 {
		t.Fatalf("rounds after flagging=%d, want 1 (no more posting)", claimer.calls.Load())
	}

	// Window closes: the verdict is dropped so next weekend starts clean.
	claimer.open = false
	manager.runWeekendClaims(context.Background(), saturday.Add(72*time.Hour))
	if _, flagged := weekendNeedsHuman[account.ID]; flagged {
		t.Fatal("closing the window must clear the needs-human verdict")
	}
}
