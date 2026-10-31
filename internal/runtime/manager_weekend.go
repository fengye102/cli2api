package runtime

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Weekend-grab sweep for ZCode promo plans, ported from angusdevgo/ZCode2api
// (read 2026-10-31). Upstream opens the weekend promo claim window Friday
// 18:00 Beijing time through Sunday midnight and hands out quota first-come;
// the manual console claim (POST /api/accounts/{id}/claim) only fires when an
// operator clicks, which loses the race. This sweep runs on the 30-second
// maintenance tick, at most every weekendClaimInterval, while the adapter
// (via the WeekendClaimer capability interface below) says the window is
// open.
//
// The capability is asserted against Adapter.Claim instead of importing the
// zcode package: internal/app's import-constraint test forbids runtime from
// referencing concrete providers (TestImportConstraints).
//
// Opt-in is the account-level AutoCheckin flag — operators already use it for
// "do background maintenance for this account", and a weekend grab without
// daily check-in makes no sense, so no new account field is introduced.

const (
	// weekendClaimInterval caps how often one account enters a grab round.
	// Upstream's reference implementation uses 10 minutes; anything faster
	// only burns claim calls that answer 1005 (名额未开/已尽).
	weekendClaimInterval = 10 * time.Minute

	// weekendClaimProvider is the only provider with a weekend promo today.
	weekendClaimProvider = "zcode"
)

// WeekendClaimer is the capability a provider's claim flow may expose for the
// weekend promo: the sweep calls AutoClaimWeekend, which skips when a promo
// plan is already active, tries the date-encoded candidate ids, and ends the
// round with Claimed/AlreadyClaimed or a retryable message.
type WeekendClaimer interface {
	AutoClaimWeekend(ctx context.Context, accountID string) (providers.ClaimResult, error)
}

// weekendClaimedAt tracks the last grab attempt per account so the 30-second
// tick does not hammer billing/claim with 1005s between rounds. Guarded by
// the maintenance loop being single-goroutine.
var weekendClaimedAt = map[string]time.Time{}

// runWeekendClaims fires one grab round per eligible account. Called from
// RunMaintenanceLoop on every 30s tick; does its own window gating by asking
// the adapter (an empty round on a closed window would also work, but the
// window check lives behind the capability, so the cheap gate here is the
// provider id plus the interval throttle).
func (manager *Manager) runWeekendClaims(ctx context.Context, now time.Time) {
	items, err := manager.store.List(ctx)
	if err != nil {
		log.Printf("weekend claim list: %v", err)
		return
	}
	for _, account := range items {
		if ctx.Err() != nil {
			return
		}
		if account.Provider != weekendClaimProvider || !account.Enabled || !account.AutoCheckin {
			continue
		}
		if last, ok := weekendClaimedAt[account.ID]; ok && now.Sub(last) < weekendClaimInterval {
			continue
		}
		weekendClaimedAt[account.ID] = now

		result, err := manager.weekendClaimAccount(ctx, account.ID)
		if err != nil {
			log.Printf("weekend claim account=%s: %v", account.ID, err)
			continue
		}
		switch {
		case result.Claimed:
			log.Printf("weekend claim account=%s: claimed %s (%s)", account.ID, result.PlanID, result.Message)
		case result.AlreadyClaimed:
			// Promo active on the account: no further rounds this weekend
			// unless the plan expires, which outlasts the window anyway.
			log.Printf("weekend claim account=%s: %s", account.ID, result.Message)
		default:
			// 1005 and friends: next round in weekendClaimInterval.
			log.Printf("weekend claim account=%s: %s", account.ID, result.Message)
		}
	}
}

// weekendClaimAccount runs one grab round through the registered adapter's
// WeekendClaimer capability, mirroring ClaimAccount's adapter plumbing.
func (manager *Manager) weekendClaimAccount(ctx context.Context, accountID string) (providers.ClaimResult, error) {
	account, err := manager.store.Get(ctx, accountID)
	if err != nil {
		return providers.ClaimResult{}, err
	}
	adapter, registered := manager.providers.Get(account.Provider)
	if !registered || adapter.Claim == nil {
		return providers.ClaimResult{}, providers.ErrUnsupported
	}
	weekend, ok := adapter.Claim.(WeekendClaimer)
	if !ok {
		return providers.ClaimResult{}, fmt.Errorf("provider %s has no weekend claimer", account.Provider)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	stop := context.AfterFunc(manager.runCtx, cancel)
	defer stop()
	return weekend.AutoClaimWeekend(ctx, accountID)
}
