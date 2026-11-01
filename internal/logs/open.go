package logs

// What is in flight right now, for the console's live panel.
//
// The recorder already persists every request and attempt, but a database row
// that is still open says nothing about the moment a request took a slot on an
// account: the row is written when the request starts, the attempt row when it
// finishes. The panel wants the middle of that window — the requests waiting
// for an account, the ones holding a slot and the model each is running — so the
// recorder keeps a small in-memory projection beside the durable rows. It is a
// projection only: nothing here is persisted, and a restart empties it, which is
// what "in flight" means anyway.

import (
	"sort"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

const (
	// maxOpenRequests bounds the projection. A leaked entry (a cancelled
	// client whose Finish never came) is dropped by the age sweep, and this cap
	// keeps the memory bounded even if that never runs.
	maxOpenRequests = 4096
	// openRequestTTL drops an entry that was never finished. Real requests last
	// minutes at most; an hour-old one is a leak, not a request.
	openRequestTTL = time.Hour
	// maxPendingCompletions bounds the finish notifications /api/live drains so
	// a console that stops polling cannot grow them without bound.
	maxPendingCompletions = 512
)

// OpenRequest is one request in flight: selecting when it has no account yet,
// bound once it took a slot.
type OpenRequest struct {
	ID             string
	RequestedModel string
	AccountID      string
	APIKeyID       string
	Status         string
	StartedAt      time.Time
	// BoundAt is when the request took a slot on AccountID; zero while it is
	// still selecting.
	BoundAt time.Time
}

// RequestCompletion is one request that left the in-flight set. The panel uses
// it to fly that cell to done/failed instead of guessing from a counter.
type RequestCompletion struct {
	RequestID string
	AccountID string
	Status    string
	At        time.Time
}

// trackStart records a request as selecting.
func (r *RequestRecorder) trackStart(log accounts.RequestLog) {
	if r == nil {
		return
	}
	now := time.Now()
	r.openMu.Lock()
	defer r.openMu.Unlock()
	if r.open == nil {
		r.open = make(map[string]*OpenRequest, 64)
	}
	r.sweepLocked(now)
	if len(r.open) >= maxOpenRequests && r.open[log.ID] == nil {
		// Drop the oldest entry rather than refuse to track the new one.
		var oldest string
		var oldestAt time.Time
		for id, item := range r.open {
			if oldest == "" || item.StartedAt.Before(oldestAt) {
				oldest, oldestAt = id, item.StartedAt
			}
		}
		delete(r.open, oldest)
	}
	r.open[log.ID] = &OpenRequest{
		ID:             log.ID,
		RequestedModel: log.RequestedModel,
		APIKeyID:       log.APIKeyID,
		Status:         accounts.RequestStatusStarted,
		StartedAt:      now,
	}
}

// BindRequest marks the account a request took a slot on. It is called from the
// executor as soon as the slot is acquired, which is the moment the panel wants
// to show; a failover rebinding the same request moves the entry to the account
// that actually served the attempt.
func (r *RequestRecorder) BindRequest(requestID, accountID string) {
	if r == nil || requestID == "" || accountID == "" {
		return
	}
	now := time.Now()
	r.openMu.Lock()
	defer r.openMu.Unlock()
	item, ok := r.open[requestID]
	if !ok {
		return
	}
	item.AccountID = accountID
	item.BoundAt = now
}

// trackFinish records how a request ended — and does nothing to the in-flight
// set when it has not ended. A streaming request is written twice: once as
// "streaming" the moment the upstream head arrives (so the console can show a
// request in progress) and once with its terminal status when the relay is
// done. The first of those must not take the request off the panel, or a
// long stream would look idle for its whole duration.
func (r *RequestRecorder) trackFinish(log accounts.RequestLog) {
	if r == nil {
		return
	}
	now := time.Now()
	status := log.Status
	if status == "" {
		// A finished request whose status never got set: report it as done
		// rather than leave the panel waiting for a resolution.
		status = accounts.RequestStatusOK
	}
	r.openMu.Lock()
	defer r.openMu.Unlock()
	item, tracked := r.open[log.ID]
	accountID := log.AccountID
	if tracked && item.AccountID != "" {
		accountID = item.AccountID
	}
	if status == accounts.RequestStatusStarted || status == accounts.RequestStatusStreaming {
		if tracked {
			item.Status = status
		}
		return
	}
	delete(r.open, log.ID)
	if len(r.completions) >= maxPendingCompletions {
		r.completions = r.completions[1:]
	}
	r.completions = append(r.completions, RequestCompletion{RequestID: log.ID, AccountID: accountID, Status: status, At: now})
}

// sweepLocked drops entries that were never finished.
func (r *RequestRecorder) sweepLocked(now time.Time) {
	for id, item := range r.open {
		if now.Sub(item.StartedAt) > openRequestTTL {
			delete(r.open, id)
		}
	}
}

// SelectingRequests lists the requests still waiting for an account, oldest
// first.
func (r *RequestRecorder) SelectingRequests() []OpenRequest {
	if r == nil {
		return nil
	}
	now := time.Now()
	r.openMu.Lock()
	defer r.openMu.Unlock()
	r.sweepLocked(now)
	out := make([]OpenRequest, 0, 8)
	for _, item := range r.open {
		if item.AccountID == "" {
			out = append(out, *item)
		}
	}
	sortOpen(out)
	return out
}

// BoundRequests lists the requests holding a slot, longest-running first.
func (r *RequestRecorder) BoundRequests() []OpenRequest {
	if r == nil {
		return nil
	}
	now := time.Now()
	r.openMu.Lock()
	defer r.openMu.Unlock()
	r.sweepLocked(now)
	out := make([]OpenRequest, 0, 16)
	for _, item := range r.open {
		if item.AccountID != "" {
			out = append(out, *item)
		}
	}
	sortOpen(out)
	return out
}

// DrainRecentCompletions returns and clears the finish notifications queued
// since the last poll: the panel animates each one, so they are handed out once.
func (r *RequestRecorder) DrainRecentCompletions() []RequestCompletion {
	if r == nil {
		return nil
	}
	r.openMu.Lock()
	defer r.openMu.Unlock()
	if len(r.completions) == 0 {
		return nil
	}
	out := r.completions
	r.completions = nil
	return out
}

// OpenRequests reports how many requests are in flight, for tests and metrics.
func (r *RequestRecorder) OpenRequests() int {
	if r == nil {
		return 0
	}
	r.openMu.Lock()
	defer r.openMu.Unlock()
	return len(r.open)
}

func sortOpen(items []OpenRequest) {
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].StartedAt.Equal(items[j].StartedAt) {
			return items[i].StartedAt.Before(items[j].StartedAt)
		}
		return items[i].ID < items[j].ID
	})
}
