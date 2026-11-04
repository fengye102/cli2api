package runtime

import (
	"testing"
	"time"
)

func TestCheckinSchedule(t *testing.T) {
	location := time.FixedZone("CST", 8*3600)
	base := time.Date(2026, 9, 19, 8, 0, 0, 0, location)
	account := Account{ID: "account", Enabled: true, AutoCheckin: true}
	if checkinDue(account, "09:00", base) {
		t.Fatal("ran before configured time")
	}
	if !checkinDue(account, "09:00", base.Add(2*time.Hour)) {
		t.Fatal("did not catch up after scheduled time")
	}
	account.LastCheckinAt = base.Add(2 * time.Hour).Format(time.RFC3339)
	for _, status := range []string{"success", "already", "skipped"} {
		account.LastCheckinStatus = status
		if checkinDue(account, "09:00", base.Add(14*time.Hour)) {
			t.Fatalf("repeated terminal status %s", status)
		}
	}
	account.LastCheckinStatus = "error"
	if !checkinDue(account, "09:00", base.Add(3*time.Hour)) {
		t.Fatal("a failed check-in was not retried on the backoff ladder")
	}
	if !checkinDue(account, "09:00", base.Add(14*time.Hour)) {
		t.Fatal("did not retry morning failure")
	}
	account.LastCheckinAt = base.Add(14 * time.Hour).Format(time.RFC3339)
	if !checkinDue(account, "09:00", base.Add(15*time.Hour)) {
		t.Fatal("a late failure was not retried on the ladder")
	}
	if checkinDue(account, "09:00", base.Add(15*time.Hour+50*time.Minute)) {
		t.Fatal("retried after the evening cutoff")
	}
	if !checkinDue(account, "09:00", base.Add(26*time.Hour)) {
		t.Fatal("previous day suppressed next day")
	}
}

func TestCheckinScheduleSameTimeAndDisabled(t *testing.T) {
	now := time.Date(2026, 9, 19, 21, 30, 0, 0, time.UTC)
	account := Account{ID: "account", Enabled: true, AutoCheckin: true, LastCheckinAt: now.Add(-10 * time.Minute).Format(time.RFC3339), LastCheckinStatus: "error"}
	if checkinDue(account, "21:00", now) {
		t.Fatal("scheduled time and retry slot must not run twice")
	}
	account.LastCheckinAt = ""
	account.AutoCheckin = false
	if checkinDue(account, "09:00", now) {
		t.Fatal("auto-checkin is opt-in")
	}
	account.AutoCheckin = true
	account.Enabled = false
	if checkinDue(account, "09:00", now) {
		t.Fatal("disabled account was scheduled")
	}
}

func TestCheckinDayUsesProviderTimezone(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 0, 1, 0, 0, location)
	if CheckedInLocalDay("2026-09-19T15:59:00Z", "success", now) {
		t.Fatal("yesterday in China was treated as today")
	}
	if !CheckedInLocalDay("2026-09-19T16:00:00Z", "already", now) {
		t.Fatal("today in China was missed")
	}
}

// TestCheckinRetryLadder pins the ladder itself: a busy upstream is retried
// after minutes, later failures wait longer, and attempt count comes from the
// day's records.
func TestCheckinRetryLadder(t *testing.T) {
	location := time.FixedZone("CST", 8*3600)
	base := time.Date(2026, 11, 4, 9, 0, 0, 0, location)
	account := Account{
		ID: "trae-1", Enabled: true, AutoCheckin: true,
		LastCheckinAt:     base.Add(time.Minute).Format(time.RFC3339Nano),
		LastCheckinStatus: "error",
		LastCheckinMsg:    "code 9074: 当前参与用户太多，请稍后再试",
	}
	if checkinDueFor(account, "09:00", base.Add(5*time.Minute), false, 1) {
		t.Fatal("retried before the first rung")
	}
	if !checkinDueFor(account, "09:00", base.Add(16*time.Minute), false, 1) {
		t.Fatal("first retry rung did not fire")
	}
	if !checkinDueFor(account, "09:00", base.Add(31*time.Minute), false, 2) {
		t.Fatal("second retry rung did not fire")
	}
	if checkinDueFor(account, "09:00", base.Add(45*time.Minute), false, 3) {
		t.Fatal("third rung fired too early")
	}
	// Ladder exhausted: only the evening slot remains.
	if checkinDueFor(account, "09:00", base.Add(11*time.Hour), false, len(checkinRetryLadder)+1) {
		t.Fatal("retried after the ladder without reaching the evening slot")
	}
	if !checkinDueFor(account, "09:00", base.Add(13*time.Hour), false, len(checkinRetryLadder)+1) {
		t.Fatal("evening slot did not catch up an exhausted ladder")
	}
	// A capped day stops completely.
	if checkinDueFor(account, "09:00", base.Add(13*time.Hour), false, checkinMaxAttempts+1) {
		t.Fatal("kept retrying past the daily attempt cap")
	}
	// Late evening is left for tomorrow so the tick cannot straddle midnight.
	if checkinDueFor(account, "09:00", base.Add(14*time.Hour+50*time.Minute), false, 1) {
		t.Fatal("retried after the evening cutoff")
	}
}

// TestCheckinRetrySkipsTerminalFailures: an answer that cannot change today must
// not be retried all day.
func TestCheckinRetrySkipsTerminalFailures(t *testing.T) {
	location := time.FixedZone("CST", 8*3600)
	base := time.Date(2026, 11, 4, 9, 0, 0, 0, location)
	for _, message := range []string{
		"今日已签到", "奖励已领取", "provider does not support check-in", "无签到接口",
	} {
		account := Account{
			ID: "a", Enabled: true, AutoCheckin: true,
			LastCheckinAt:     base.Add(time.Minute).Format(time.RFC3339Nano),
			LastCheckinStatus: "error",
			LastCheckinMsg:    message,
		}
		if checkinDueFor(account, "09:00", base.Add(3*time.Hour), false, 1) {
			t.Fatalf("retried terminal failure %q", message)
		}
	}
	// Transient wordings keep retrying.
	for _, message := range []string{
		"代码 9074: 当前参与用户太多，请稍后再试",
		"今日未见积分入账（发放触发未生效，稍后重试）",
		"upstream busy, try again later",
	} {
		account := Account{
			ID: "a", Enabled: true, AutoCheckin: true,
			LastCheckinAt:     base.Add(time.Minute).Format(time.RFC3339Nano),
			LastCheckinStatus: "error",
			LastCheckinMsg:    message,
		}
		if !checkinDueFor(account, "09:00", base.Add(20*time.Minute), false, 1) {
			t.Fatalf("gave up on transient failure %q", message)
		}
	}
}
