package providers

import "context"

// ClaimPlan is a claimable plan reported by a provider's claim preview.
type ClaimPlan struct {
	PlanID   string `json:"plan_id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	StartsAt int64  `json:"starts_at,omitempty"`
	EndsAt   int64  `json:"ends_at,omitempty"`
}

// ClaimResult is the outcome of a claim round.
type ClaimResult struct {
	Claimed        bool   `json:"claimed"`
	AlreadyClaimed bool   `json:"already_claimed,omitempty"`
	PlanID         string `json:"plan_id,omitempty"`
	Message        string `json:"message,omitempty"`
	ResetAt        string `json:"reset_at,omitempty"`
}

// AccountClaimer previews and claims provider plans (e.g. ZCode's daily
// Start-Plan trust builds). Providers without claimable plans leave the
// Adapter.Claim field nil.
type AccountClaimer interface {
	// PreviewClaim lists the claimable plans for an account.
	PreviewClaim(ctx context.Context, accountID string) ([]ClaimPlan, error)
	// Claim claims the named plan for an account.
	Claim(ctx context.Context, accountID, planID string) (ClaimResult, error)
}
