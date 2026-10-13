package raccoon

import (
	"context"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Quota reports the account's usage as best the upstream allows. Raccoon's
// web gateway does not expose an account-level balance in a stable shape, so
// this returns a neutral report (no windows, no invented totals) instead of
// guessing. The console card then shows the plan-independent "no quota data"
// state rather than a misleading zero.
func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	return &providers.QuotaInfo{
		FetchedAt:  nowString(),
		ProviderID: "raccoon",
	}, nil
}

func nowString() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z07:00")
}
