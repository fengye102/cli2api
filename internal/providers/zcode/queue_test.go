package zcode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

func TestQueueNoticeBody(t *testing.T) {
	queued := []string{
		`{"code":3008,"msg":"user concurrency limit exceeded"}`,
		`{"code": 3009,"msg":"too many concurrent requests"}`,
		`{"error":{"message":"concurrent request limit"}}`,
		`{"msg":"请求排队中"}`,
	}
	for _, body := range queued {
		if !queueNoticeBody([]byte(body)) {
			t.Errorf("not detected as queued: %s", body)
		}
	}

	// Everything else must keep its own reading: a 3001 is "device mid
	// missing", a 3012 is risk control, and a quota envelope is quota.
	notQueued := []string{
		``,
		`{"code":3001,"msg":"x-device-mid required"}`,
		`{"code":3012,"msg":"request has been blocked due to unusual activity"}`,
		`{"code":1005,"msg":"exceed quota limit"}`,
		`{"error":{"type":"rate_limit_error","message":"slow down"}}`,
		`all accounts are cooling down`,
	}
	for _, body := range notQueued {
		if queueNoticeBody([]byte(body)) {
			t.Errorf("false positive: %s", body)
		}
	}
}

func TestRetryQueuedTurnSleepsAndRetries(t *testing.T) {
	c := &Client{}
	ctx := context.Background()

	// Attempt 0 with a queue notice: the turn is re-sent.
	start := time.Now()
	retry, err := c.retryQueuedTurn(ctx, 0, 405, []byte(`{"code":3008,"msg":"user concurrency limit exceeded"}`))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !retry {
		t.Fatal("first queue notice must retry")
	}
	if elapsed := time.Since(start); elapsed < queueRetryDelay(0) {
		t.Fatalf("retried without waiting: %s", elapsed)
	}

	// A non-queue answer is never retried and never waits.
	start = time.Now()
	retry, err = c.retryQueuedTurn(ctx, 0, 405, []byte(`{"code":3012,"msg":"unusual activity"}`))
	if err != nil || retry {
		t.Fatalf("risk retried: retry=%v err=%v", retry, err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("non-queue answer slept for %s", elapsed)
	}

	// Once the attempts are spent the queue surfaces as a busy signal.
	retry, err = c.retryQueuedTurn(ctx, zcodeQueueAttempts, 405, []byte(`{"code":3008}`))
	if err != nil || retry {
		t.Fatalf("attempt %d retried: retry=%v err=%v", zcodeQueueAttempts, retry, err)
	}
}

func TestRetryQueuedTurnHonoursCancellation(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retry, err := c.retryQueuedTurn(ctx, 0, 405, []byte(`{"code":3009}`))
	if retry {
		t.Fatal("canceled context must not retry")
	}
	if err == nil {
		t.Fatal("canceled context must surface its error")
	}
}

// TestClassify_ConcurrencyQueueNamesTheCause keeps the exhausted-queue reading
// distinct from quota and auth, so an operator fixes concurrency instead of
// re-importing a healthy credential.
func TestClassify_ConcurrencyQueueNamesTheCause(t *testing.T) {
	got := Classify(405, `{"code":3008,"msg":"user concurrency limit exceeded"}`)
	if got.Kind != accounts.KindRateLimit {
		t.Fatalf("kind=%q want %q", got.Kind, accounts.KindRateLimit)
	}
	if !strings.Contains(got.Message, "concurrency queue") {
		t.Fatalf("message=%q", got.Message)
	}
	if !strings.Contains(got.Message, "3008") {
		t.Fatalf("message lost the business code: %q", got.Message)
	}
	// A 3001 (missing device mid) is neither queue nor auth: it stays a plain
	// 405 that the caller reads as risk/busy, never as a dead credential.
	if got := Classify(405, `{"code":3001,"msg":"x-device-mid required"}`); got.Kind == accounts.KindAuth {
		t.Fatalf("3001 classified as auth: %+v", got)
	}
}

func TestQueueRetryDelayGrowsAndStaysBounded(t *testing.T) {
	if queueRetryDelay(0) >= queueRetryDelay(1) || queueRetryDelay(1) >= queueRetryDelay(2) {
		t.Fatal("delay must grow with the attempt")
	}
	if queueRetryDelay(99) > 5*time.Second {
		t.Fatalf("delay unbounded: %s", queueRetryDelay(99))
	}
}
