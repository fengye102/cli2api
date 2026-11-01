package console

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/executor"
)

// LiveAccount is the per-account slice of the live concurrency snapshot
// (LiveStrip / sidebar). Kept for backward compatibility.
type LiveAccount struct {
	ID          string `json:"id"`
	InFlight    int    `json:"in_flight"`
	MaxInFlight int    `json:"max_inflight"`
	Enabled     bool   `json:"enabled"`
}

// LiveWorker is one account-pool slot projected for the overview's live card:
// the pool's occupancy, whether the account is resting, and how long it has
// been busy.
type LiveWorker struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	Busy        bool    `json:"busy"`
	InFlight    int     `json:"in_flight"`
	MaxInFlight int     `json:"max_inflight"`
	Enabled     bool    `json:"enabled"`
	RateLimited bool    `json:"rate_limited"`
	Progress    float64 `json:"progress"`
	// StartedAt is when the slot went busy (the oldest bound request on this
	// account), so the card shows real elapsed time rather than a guess.
	StartedAt string `json:"started_at,omitempty"`
}

// LiveRateLimit is one active cooldown window: account-wide (the pool's
// DownUntil) or scoped to one model (ModelDownUntil). The console reads them
// from the account view and never keeps a second cooldown state machine.
type LiveRateLimit struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	AccountID   string `json:"account_id"`
	Model       string `json:"model,omitempty"`
	Until       string `json:"until"`
	RemainingMs int64  `json:"remaining_ms"`
}

// LiveJob is one request in the live set: a request still selecting an account
// (queue) or one holding a slot (occupancy).
type LiveJob struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at,omitempty"`
	// AccountID is empty on a queued request and always set on an occupied one.
	AccountID string `json:"account_id,omitempty"`
}

// LiveCompletion is one request that left the live set since the last poll, so
// the card can settle that cell instead of inferring it from a counter.
type LiveCompletion struct {
	RequestID string `json:"request_id,omitempty"`
	AccountID string `json:"account_id"`
	Status    string `json:"status"`
}

// LiveSnapshot is the cheap payload behind /api/live, polled every few seconds
// by the overview live strip and its card.
type LiveSnapshot struct {
	Time              string                        `json:"time"`
	InFlight          int                           `json:"in_flight"`
	Capacity          int                           `json:"capacity"`
	RPM               int                           `json:"rpm"`
	Accounts          []LiveAccount                 `json:"accounts"`
	Workers           []LiveWorker                  `json:"workers"`
	RateLimits        []LiveRateLimit               `json:"rate_limits"`
	Sticky            executor.SessionAffinityStats `json:"sticky"`
	RecentCompletions []LiveCompletion              `json:"recent_completions,omitempty"`
	Queue             []LiveJob                     `json:"queue,omitempty"`
	Occupancy         []LiveJob                     `json:"occupancy,omitempty"`
}

// buildLiveSnapshot sums in-flight over all accounts and capacity (the
// effective max_inflight, default 4) over enabled accounts only, then projects
// workers and rate limits for the live card. It is a read-only projection: the
// classify→pool write path is untouched.
func buildLiveSnapshot(views []accounts.AccountView, rpm int, sticky executor.SessionAffinityStats, bound []LiveJob, now time.Time) LiveSnapshot {
	out := LiveSnapshot{
		Time:       now.Format(time.RFC3339),
		RPM:        rpm,
		Accounts:   make([]LiveAccount, 0, len(views)),
		Workers:    make([]LiveWorker, 0, len(views)),
		RateLimits: make([]LiveRateLimit, 0),
		Queue:      make([]LiveJob, 0),
		Occupancy:  make([]LiveJob, 0),
		Sticky:     sticky,
	}
	// The oldest bound request on an account is when its slot went busy.
	busySince := make(map[string]time.Time, len(bound))
	for _, job := range bound {
		started, err := time.Parse(time.RFC3339, strings.TrimSpace(job.StartedAt))
		if err != nil || job.AccountID == "" {
			continue
		}
		if was, ok := busySince[job.AccountID]; !ok || started.Before(was) {
			busySince[job.AccountID] = started
		}
	}
	for _, v := range views {
		maxInFlight := accounts.DefaultMaxInFlightValue(v.MaxInFlight)
		out.InFlight += v.InFlight
		if v.Enabled {
			out.Capacity += maxInFlight
		}
		out.Accounts = append(out.Accounts, LiveAccount{
			ID: v.ID, InFlight: v.InFlight, MaxInFlight: maxInFlight, Enabled: v.Enabled,
		})

		name := strings.TrimSpace(v.Name)
		if name == "" {
			name = v.ID
		}
		accountLimited := cooldownActive(v.DownUntil, now)
		modelLimited := false
		for model, until := range v.ModelCooldowns {
			if !cooldownActive(until, now) {
				continue
			}
			modelLimited = true
			label := name
			if model != "" {
				label = fmt.Sprintf("%s · %s", name, model)
			}
			out.RateLimits = append(out.RateLimits, LiveRateLimit{
				ID:          v.ID + ":" + model,
				Label:       label,
				AccountID:   v.ID,
				Model:       model,
				Until:       until,
				RemainingMs: remainingMs(until, now),
			})
		}
		if accountLimited {
			out.RateLimits = append(out.RateLimits, LiveRateLimit{
				ID:          v.ID,
				Label:       name,
				AccountID:   v.ID,
				Until:       v.DownUntil,
				RemainingMs: remainingMs(v.DownUntil, now),
			})
		}

		progress := 0.0
		if maxInFlight > 0 && v.InFlight > 0 {
			progress = float64(v.InFlight) / float64(maxInFlight)
			if progress > 1 {
				progress = 1
			}
		}
		worker := LiveWorker{
			ID: v.ID, Name: name, Provider: v.Provider,
			Busy: v.InFlight > 0, InFlight: v.InFlight, MaxInFlight: maxInFlight,
			Enabled: v.Enabled, RateLimited: accountLimited || modelLimited,
			Progress: progress,
		}
		if since, ok := busySince[v.ID]; ok && v.InFlight > 0 {
			worker.StartedAt = since.UTC().Format(time.RFC3339)
		}
		out.Workers = append(out.Workers, worker)
	}
	sort.SliceStable(out.RateLimits, func(i, j int) bool {
		if out.RateLimits[i].RemainingMs != out.RateLimits[j].RemainingMs {
			return out.RateLimits[i].RemainingMs > out.RateLimits[j].RemainingMs
		}
		return out.RateLimits[i].ID < out.RateLimits[j].ID
	})
	return out
}

// cooldownActive reports whether a cooldown timestamp is still in the future.
func cooldownActive(until string, now time.Time) bool {
	until = strings.TrimSpace(until)
	if until == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return false
	}
	return now.Before(t)
}

// remainingMs is how long the cooldown still has to run.
func remainingMs(until string, now time.Time) int64 {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(until))
	if err != nil || !now.Before(t) {
		return 0
	}
	return t.Sub(now).Milliseconds()
}

func (h *Handler) stickyStats() executor.SessionAffinityStats {
	if h == nil || h.Executor == nil || h.Executor.SessionAffinity == nil {
		return executor.SessionAffinityStats{}
	}
	return h.Executor.SessionAffinity.Stats()
}

// liveJobs projects the recorder's in-flight set into the card's two lists.
func (h *Handler) liveJobs() (queue, occupancy []LiveJob) {
	if h == nil || h.Recorder == nil {
		return nil, nil
	}
	for _, item := range h.Recorder.SelectingRequests() {
		name := strings.TrimSpace(item.RequestedModel)
		if name == "" {
			name = item.ID
		}
		queue = append(queue, LiveJob{
			ID:        item.ID,
			Name:      name,
			Status:    item.Status,
			StartedAt: item.StartedAt.UTC().Format(time.RFC3339),
			AccountID: strings.TrimSpace(item.AccountID),
		})
	}
	for _, item := range h.Recorder.BoundRequests() {
		name := strings.TrimSpace(item.RequestedModel)
		if name == "" {
			name = item.ID
		}
		started := item.BoundAt
		if started.IsZero() {
			started = item.StartedAt
		}
		status := strings.TrimSpace(item.Status)
		if status == "" {
			status = accounts.RequestStatusStarted
		}
		occupancy = append(occupancy, LiveJob{
			ID:        item.ID,
			Name:      name,
			Status:    status,
			StartedAt: started.UTC().Format(time.RFC3339),
			AccountID: strings.TrimSpace(item.AccountID),
		})
	}
	return queue, occupancy
}

func (h *Handler) HandleLive(w http.ResponseWriter, r *http.Request) {
	views, err := h.Control.Accounts.List(r.Context(), false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "account_list_failed", err.Error())
		return
	}
	rpm := 0
	if h.Recorder != nil {
		rpm = h.Recorder.RecentRequests()
	}
	queue, occupancy := h.liveJobs()
	snap := buildLiveSnapshot(views, rpm, h.stickyStats(), occupancy, time.Now())
	snap.Queue = queue
	snap.Occupancy = occupancy
	if h.Recorder != nil {
		for _, c := range h.Recorder.DrainRecentCompletions() {
			snap.RecentCompletions = append(snap.RecentCompletions, LiveCompletion{
				RequestID: c.RequestID,
				AccountID: c.AccountID,
				Status:    c.Status,
			})
		}
	}
	writeJSON(w, http.StatusOK, snap)
}
