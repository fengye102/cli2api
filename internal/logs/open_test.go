package logs

import (
	"context"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// fakePersister satisfies the write surface the recorder uses; the live
// projection never touches it.
type fakePersister struct{}

func (fakePersister) InsertRequestLog(context.Context, accounts.RequestLog) error { return nil }
func (fakePersister) UpdateRequestLog(context.Context, accounts.RequestLog) error { return nil }
func (fakePersister) InsertRequestAttempt(context.Context, accounts.RequestAttempt) error {
	return nil
}
func (fakePersister) InsertRequestStreamDiagnostic(context.Context, accounts.RequestStreamDiagnostic) error {
	return nil
}
func (fakePersister) InsertRequestUsageDetail(context.Context, accounts.RequestUsageDetail) error {
	return nil
}
func (fakePersister) PurgeRequestLogs(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}

func newTestRecorder(t *testing.T) *RequestRecorder {
	t.Helper()
	recorder := NewRequestRecorder(fakePersister{})
	t.Cleanup(recorder.Close)
	return recorder
}

// TestLiveTracksSelectingThenBoundThenDone walks the three stages the panel
// shows: waiting for an account, holding a slot, gone.
func TestLiveTracksSelectingThenBoundThenDone(t *testing.T) {
	recorder := newTestRecorder(t)
	recorder.Start(accounts.RequestLog{ID: "r1", RequestedModel: "glm-5.3-flash", Status: accounts.RequestStatusStarted})
	if got := recorder.OpenRequests(); got != 1 {
		t.Fatalf("open = %d want 1", got)
	}
	selecting := recorder.SelectingRequests()
	if len(selecting) != 1 || selecting[0].ID != "r1" || selecting[0].RequestedModel != "glm-5.3-flash" {
		t.Fatalf("selecting = %+v", selecting)
	}
	if bound := recorder.BoundRequests(); len(bound) != 0 {
		t.Fatalf("a request that has not taken a slot is not bound: %+v", bound)
	}

	recorder.BindRequest("r1", "acc_a")
	bound := recorder.BoundRequests()
	if len(bound) != 1 || bound[0].AccountID != "acc_a" {
		t.Fatalf("bound = %+v", bound)
	}
	if bound[0].BoundAt.IsZero() {
		t.Error("a bound request records when it took the slot")
	}
	if len(recorder.SelectingRequests()) != 0 {
		t.Error("a bound request is no longer selecting")
	}

	// A failover moves the same request to the account that served it.
	recorder.BindRequest("r1", "acc_b")
	if bound := recorder.BoundRequests(); len(bound) != 1 || bound[0].AccountID != "acc_b" {
		t.Fatalf("rebind = %+v", bound)
	}

	recorder.Finish(accounts.RequestLog{ID: "r1", AccountID: "acc_b", Status: accounts.RequestStatusOK})
	if got := recorder.OpenRequests(); got != 0 {
		t.Fatalf("finished request still open: %d", got)
	}
	done := recorder.DrainRecentCompletions()
	if len(done) != 1 || done[0].RequestID != "r1" || done[0].AccountID != "acc_b" || done[0].Status != accounts.RequestStatusOK {
		t.Fatalf("completions = %+v", done)
	}
	// Drained once: the panel animates each finish a single time.
	if again := recorder.DrainRecentCompletions(); again != nil {
		t.Fatalf("second drain = %+v", again)
	}
}

// TestLiveBindWithoutStartIsIgnored keeps a stray bind from inventing work.
func TestLiveBindWithoutStartIsIgnored(t *testing.T) {
	recorder := newTestRecorder(t)
	recorder.BindRequest("ghost", "acc_a")
	if len(recorder.BoundRequests()) != 0 || recorder.OpenRequests() != 0 {
		t.Fatal("a bind for an unknown request must not create one")
	}
	recorder.BindRequest("", "acc_a")
	recorder.BindRequest("r1", "")
	if recorder.OpenRequests() != 0 {
		t.Fatal("empty ids must be ignored")
	}
}

// TestLiveDropsALeakedRequest: a request whose finish never came (a killed
// client) must not sit in the panel forever.
func TestLiveDropsALeakedRequest(t *testing.T) {
	recorder := newTestRecorder(t)
	recorder.Start(accounts.RequestLog{ID: "leak", RequestedModel: "glm-5.3"})
	recorder.openMu.Lock()
	recorder.open["leak"].StartedAt = time.Now().Add(-2 * openRequestTTL)
	recorder.openMu.Unlock()
	if got := recorder.SelectingRequests(); len(got) != 0 {
		t.Fatalf("stale request still listed: %+v", got)
	}
	if got := recorder.OpenRequests(); got != 0 {
		t.Fatalf("stale request still open: %d", got)
	}
}

// TestLiveCapsPendingCompletions: a console that stops polling must not grow
// the completion list without bound.
func TestLiveCapsPendingCompletions(t *testing.T) {
	recorder := newTestRecorder(t)
	for i := 0; i < maxPendingCompletions+50; i++ {
		recorder.Start(accounts.RequestLog{ID: "r", RequestedModel: "m"})
		recorder.Finish(accounts.RequestLog{ID: "r", Status: accounts.RequestStatusOK})
	}
	if got := len(recorder.DrainRecentCompletions()); got != maxPendingCompletions {
		t.Fatalf("pending completions = %d want %d", got, maxPendingCompletions)
	}
}

// TestLiveListsOldestFirst keeps the panel's order stable between polls.
func TestLiveListsOldestFirst(t *testing.T) {
	recorder := newTestRecorder(t)
	recorder.Start(accounts.RequestLog{ID: "new"})
	recorder.openMu.Lock()
	recorder.open["new"].StartedAt = time.Now()
	recorder.open["old"] = &OpenRequest{ID: "old", StartedAt: time.Now().Add(-time.Minute), Status: accounts.RequestStatusStarted}
	recorder.openMu.Unlock()
	got := recorder.SelectingRequests()
	if len(got) != 2 || got[0].ID != "old" {
		t.Fatalf("order = %+v", got)
	}
}
