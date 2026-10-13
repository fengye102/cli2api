package antigravity

import (
	"context"
	"fmt"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Quota reports account usage. Gemini API keys have no public balance or
// quota endpoint, so the adapter reports a neutral, non-exhausted view: the
// console renders usage as unknown and routing never flips an account to
// exhausted on quota alone.
func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !credential.Ready() {
		return nil, fmt.Errorf("antigravity credential incomplete; paste a Gemini API key")
	}
	return &providers.QuotaInfo{
		Unit:       QuotaUnit,
		Exceeded:   false,
		ProviderID: "antigravity",
	}, nil
}
