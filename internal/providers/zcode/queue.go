package zcode

import (
	"context"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// The plan gateway queues a turn instead of refusing it when the same user (or
// the same egress) already holds its allowed concurrency. Upstream's plugin
// treats this as "wait, then re-send on the same account" — three concurrent
// turns from one egress is already enough to be told 3008, and the queue clears
// in seconds.
//
// Cooling the account instead is what turns one busy moment into an outage:
// every other request for that account then fails fast for the whole cooldown
// window, which reads in the console as "the channel is down" while the
// credential is perfectly healthy.
const (
	// zcodeQueueAttempts is how many times one turn is re-sent before the queue
	// is reported as a busy signal.
	zcodeQueueAttempts = 3
)

var zcodeQueueMarkers = []string{
	`"code":3008`,
	`"code":3009`,
	`"code": 3008`,
	`"code": 3009`,
	"concurrency limit",
	"concurrent request",
	"too many concurrent",
	"user concurrency",
	"并发数",
	"排队",
}

// queueNoticeBody reports whether an upstream body says the turn was queued
// rather than refused. Both the 405 status the gateway wraps it in and the 2xx
// envelope form are covered, because the gateway is inconsistent about which it
// uses.
func queueNoticeBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	text := strings.ToLower(string(body))
	for _, marker := range zcodeQueueMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// queueRetryDelay is the wait before re-sending. Short and growing, and bounded
// so a turn never sits in the executor's slot for long.
func queueRetryDelay(attempt int) time.Duration {
	switch {
	case attempt <= 0:
		return 2 * time.Second
	case attempt == 1:
		return 3 * time.Second
	default:
		return 5 * time.Second
	}
}

// sleepCtx waits, honouring the request's cancellation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// queueClassified is the decorator applied when the queue outlasts the retries:
// a rate-limit-shaped busy signal whose message names the cause, so the operator
// reads "concurrency queue" and not "quota" or "auth".
func queueClassified(status int, body string) providers.ClassifiedError {
	return providers.ClassifiedError{
		Kind:   accounts.KindRateLimit,
		Status: firstNonEmptyStatus(status, 429),
		Message: "ZCode concurrency queue (3008/3009): the same egress already holds " +
			"its allowed concurrent turns; the turn was re-sent " +
			"before giving up. Lower the account's concurrency or add an egress; body=" +
			edgeSnippet(body),
	}
}

// retryQueuedTurn decides one attempt's fate. It reports shouldRetry=true after
// sleeping off a queue notice, so the caller re-sends the same turn on the same
// account; it never touches the verification pool, because a queue notice says
// nothing about the credential or the captcha. Every other answer, including a
// queue that outlasted zcodeQueueAttempts, returns false and is read by the
// caller as usual.
func (c *Client) retryQueuedTurn(ctx context.Context, attempt, status int, body []byte) (bool, error) {
	if attempt >= zcodeQueueAttempts || !queueNoticeBody(body) {
		return false, nil
	}
	if err := sleepCtx(ctx, queueRetryDelay(attempt)); err != nil {
		return false, err
	}
	return true, nil
}
