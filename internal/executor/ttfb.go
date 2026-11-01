package executor

import (
	"math"
	"sort"
	"time"
)

// First-byte latency memory behind the lowest-ttfb routing strategy.
//
// The reverse-engineered channels (raccoon, zcode, autoclaw) answer in a few
// hundred milliseconds on one account and stall for ten seconds on another for
// the very same public model. Failover cannot help: the request is not failing,
// it is slow. Racing two accounts in parallel (hedged requests) would help the
// user but bill the loser, and these upstreams bill when a request is accepted,
// not when it finishes - so a race spends credits twice to save a few seconds.
//
// Instead of racing, remember which accounts have actually answered fast lately
// and stop rotating the route into the ones that have not. That costs nothing
// extra upstream: it only changes which account gets the next request.
//
// Nothing here is persisted. The window is process-local and rebuilds itself
// within a few requests per account, so a cold start must behave exactly like
// round-robin - which is what the unsampled prior below is for.
const (
	// ttfbAlpha is the EWMA weight of a new sample. 0.3 converges in about
	// three requests: fast enough to demote a stalling account, slow enough
	// that one lucky sample cannot promote one.
	ttfbAlpha = 0.3
	// ttfbPriorMs is the score of an account without a fresh sample. It sits
	// between "healthy" and "clearly stalling": a new account must still be
	// tried or it never gathers samples, but it must not outrank an account
	// that has been answering in 300ms.
	ttfbPriorMs = 1200
	// ttfbTTL is how long a sample counts as evidence. An account that was
	// fast ten minutes ago may be stalled now; letting the sample expire puts
	// it back on the prior instead of keeping it promoted forever.
	ttfbTTL = 10 * time.Minute
	// ttfbFloorMs drops readings that cannot be a real first byte, so an
	// empty or locally-failed attempt cannot drag an average down.
	ttfbFloorMs = 20
	// ttfbBandPct and ttfbBandMs widen the winning band: every account within
	// 25% (plus 150ms) of the best stays a candidate, so rotation keeps
	// spreading load instead of herding a whole route onto one account.
	ttfbBandPct = 1.25
	ttfbBandMs  = 150.0
	// ttfbOutlierMult winsorizes a sample at this multiple of the current
	// estimate. One 9s stall on a 300ms account then reads as 900ms instead
	// of dragging the average to 2.9s and exiling a healthy account over one
	// hiccup. A sustained slowdown still climbs, because the ceiling moves up
	// with the estimate on every sample.
	ttfbOutlierMult = 3.0
	// stickyEscapeGapMs is the absolute first-byte gap that justifies breaking
	// session affinity. A pinned account is worth keeping for its cache
	// locality and its stable behaviour, so a merely slower one must not churn
	// the session; only a gap this large (seconds, not milliseconds) buys back
	// more than the move costs.
	stickyEscapeGapMs = 2000.0
)

// ttfbSample is one account's smoothed first-byte estimate.
type ttfbSample struct {
	ms float64
	at time.Time
}

// ObserveTTFB records one successful first-byte measurement. Only successful
// attempts call it: a timeout is already handled by the cooldown path, and
// folding timeouts into this average would count the same failure twice.
func (p *Pool) ObserveTTFB(id string, ms int) {
	if p == nil || id == "" || ms < ttfbFloorMs {
		return
	}
	value := float64(ms)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ttfb == nil {
		p.ttfb = make(map[string]ttfbSample)
	}
	sample, seen := p.ttfb[id]
	if !seen || now.Sub(sample.at) > ttfbTTL {
		p.ttfb[id] = ttfbSample{ms: value, at: now}
		return
	}
	if value > ttfbOutlierMult*sample.ms {
		value = ttfbOutlierMult * sample.ms
	}
	p.ttfb[id] = ttfbSample{ms: sample.ms + ttfbAlpha*(value-sample.ms), at: now}
}

// TTFB reports the current estimate for an account and whether a fresh sample
// backs it. The console shows what the scheduler believes; tests assert on it.
func (p *Pool) TTFB(id string) (int, bool) {
	if p == nil || id == "" {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sample, ok := p.ttfb[id]
	if !ok || time.Since(sample.at) > ttfbTTL {
		return 0, false
	}
	return int(math.Round(sample.ms)), true
}

// ttfbScore is what the strategy ranks on: the account's own fresh estimate, or
// the pool prior when it has none. Must be called with p.mu held.
func (p *Pool) ttfbScore(id string, now time.Time, prior float64) float64 {
	sample, ok := p.ttfb[id]
	if !ok || now.Sub(sample.at) > ttfbTTL {
		return prior
	}
	return sample.ms
}

// ttfbPrior is the median of the fresh samples, or ttfbPriorMs when the pool
// has none. The median rather than the mean: one stalled account must not drag
// every unsampled account up with it. Must be called with p.mu held.
func (p *Pool) ttfbPrior(now time.Time) float64 {
	values := make([]float64, 0, len(p.ttfb))
	for _, sample := range p.ttfb {
		if now.Sub(sample.at) <= ttfbTTL {
			values = append(values, sample.ms)
		}
	}
	if len(values) == 0 {
		return ttfbPriorMs
	}
	sort.Float64s(values)
	return values[len(values)/2]
}

// StickyTooSlow reports whether a session's bound account has drifted far
// enough behind a ready alternative to justify dropping the binding.
//
// This is the gap the latency band cannot close on its own: a session is pinned
// before selection runs, so a bound account that goes slow keeps the whole
// conversation for the affinity TTL - on a real deployment that was 92% of all
// traffic, with one 19s account holding 128 requests an hour. Escaping here
// costs nothing upstream; the next successful attempt re-binds the session.
//
// Both the bound account and the alternative need a fresh sample. An account we
// know nothing about is not evidence of slowness, and guessing would move
// sessions on noise.
func (p *Pool) StickyTooSlow(accountID, publicModel string) bool {
	if p == nil || accountID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Only the latency-aware strategy may move sessions on latency. The other
	// strategies keep the promise that enabling them changes nothing else, so a
	// deployment on round-robin must not start reshuffling conversations just
	// because samples happen to exist.
	if p.routingStrategy != RoutingStrategyLowestTTFB {
		return false
	}
	now := time.Now()
	bound, ok := p.ttfb[accountID]
	if !ok || now.Sub(bound.at) > ttfbTTL {
		return false
	}
	// The alternative has to be a candidate the picker would accept, not just a
	// fast account: escaping towards an account that cannot serve the model
	// would trade a slow answer for no answer.
	q := RouteQuery{PublicModel: publicModel}
	best := math.Inf(1)
	found := false
	for i := range p.items {
		item := p.items[i]
		if item.ID == accountID || !routeMatches(item, q) {
			continue
		}
		if itemDown(item, now) || itemModelDown(item, q, now) || itemSaturated(item) {
			continue
		}
		sample, ok := p.ttfb[item.ID]
		if !ok || now.Sub(sample.at) > ttfbTTL {
			continue
		}
		if sample.ms < best {
			best, found = sample.ms, true
		}
	}
	if !found {
		return false
	}
	return bound.ms > best+stickyEscapeGapMs
}

// ttfbCandidates narrows the ready set to the accounts that have been answering
// first-byte near the pool's best lately. Must be called with p.mu held.
//
// The result is a band, not a single winner, because the caller rotates inside
// it: two accounts that both answer in ~300ms keep sharing the route, while one
// that answers in 10s drops out until it produces a fresh fast sample.
func (p *Pool) ttfbCandidates(available []Item, now time.Time) []Item {
	if len(available) < 2 {
		return available
	}
	prior := p.ttfbPrior(now)
	scores := make([]float64, len(available))
	best := math.Inf(1)
	for i, item := range available {
		scores[i] = p.ttfbScore(item.ID, now, prior)
		if scores[i] < best {
			best = scores[i]
		}
	}
	limit := best*ttfbBandPct + ttfbBandMs
	band := make([]Item, 0, len(available))
	for i, item := range available {
		if scores[i] <= limit {
			band = append(band, item)
		}
	}
	return band
}
