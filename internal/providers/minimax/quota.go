package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// creditUnit is the console unit for MiniMax Code credits. One credit is the
// billing unit the agent gateway deducts from; the packs themselves are what
// daily grants and purchases add up to.
const creditUnit = "credit"

// creditDetails is the only quota surface the account exposes: one row per
// granted pack, each with its own 30-day expiry. There is no aggregate
// endpoint, so the totals are summed here.
type creditDetails struct {
	Details []struct {
		CreditType      flexNumber `json:"credit_type"`
		GrantedAmount   flexNumber `json:"granted_amount"`
		RemainingAmount flexNumber `json:"remaining_amount"`
		ConsumedAmount  flexNumber `json:"consumed_amount"`
		GrantedAtMS     flexNumber `json:"granted_at_ms"`
		ExpireAtMS      flexNumber `json:"expire_at_ms"`
	} `json:"details"`
	TotalCount flexNumber `json:"total_count"`
	BaseResp   struct {
		StatusCode flexNumber `json:"status_code"`
		StatusMsg  string     `json:"status_msg"`
	} `json:"base_resp"`
}

// flexNumber accepts the gateway's mixed encoding: the same field arrives as a
// quoted decimal ("400.00") on one pack and as a bare number on the next.
type flexNumber float64

func (f *flexNumber) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*f = 0
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		unquoted, err := strconv.Unquote(text)
		if err != nil {
			return err
		}
		text = strings.TrimSpace(unquoted)
		if text == "" {
			*f = 0
			return nil
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fmt.Errorf("minimax quota: %q is not a number", text)
	}
	*f = flexNumber(value)
	return nil
}

func (f flexNumber) float() float64 { return float64(f) }

// fetchCreditDetails reads the pack list for one account.
func (c *Client) fetchCreditDetails(ctx context.Context, accountID string, cred Credential) ([]byte, error) {
	site := c.site(cred.Region)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, site.AgentHost+CreditDetailsPath, nil)
	if err != nil {
		return nil, err
	}
	applyAuthHeaders(req.Header, cred)
	req.Header.Set("Accept", "application/json")
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimax 额度不可达：%w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("minimax 额度读取失败：%w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("minimax 额度鉴权失败 HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("minimax 额度 HTTP %d：%s", resp.StatusCode, clip(string(body), 200))
	}
	return body, nil
}

// Quota reports the credits left across the account's packs. Exceeded is set
// when nothing is left, which lets routing stop picking an account before the
// next chat request fails with the upstream 1008 refusal.
func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	cred, err := c.freshCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	quotaCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	body, err := c.fetchCreditDetails(quotaCtx, accountID, cred)
	if err != nil {
		return nil, err
	}
	return parseCreditDetails(body, c.clock())
}

func parseCreditDetails(body []byte, now time.Time) (*providers.QuotaInfo, error) {
	var payload creditDetails
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("minimax 额度响应无法解析：%w", err)
	}
	// status_msg is prose, not a contract: a healthy response says "success" on
	// one call and "ok" on the next. Only the code decides.
	if code := payload.BaseResp.StatusCode.float(); code != 0 {
		return nil, fmt.Errorf("minimax 额度失败：code=%v %s", code, strings.TrimSpace(payload.BaseResp.StatusMsg))
	}

	info := &providers.QuotaInfo{
		Unit:       creditUnit,
		ProviderID: ProviderID,
		FetchedAt:  now.UTC().Format(time.RFC3339),
	}
	var (
		expiresAt      int64
		expiringRemain float64
	)
	for _, pack := range payload.Details {
		size := pack.GrantedAmount.float()
		remain := pack.RemainingAmount.float()
		used := pack.ConsumedAmount.float()
		if used == 0 && size > 0 && remain <= size {
			// The gateway omits consumed_amount on some packs; derive it so the
			// console does not show a pack that is 100% remaining and 53% used.
			used = size - remain
		}
		if used < 0 {
			used = 0
		}
		endsAt := int64(pack.ExpireAtMS.float() / 1000)
		info.Total += size
		info.Remaining += remain
		info.Used += used
		info.Packages = append(info.Packages, providers.QuotaPackage{
			Remain: remain,
			Used:   used,
			Size:   size,
			Unit:   creditUnit,
			EndsAt: endsAt,
		})
		if remain > 0 && endsAt > 0 && (expiresAt == 0 || endsAt < expiresAt) {
			// The soonest expiry is the one the operator has to care about:
			// leftover credits are gone at that point, not at the last expiry.
			expiresAt = endsAt
			expiringRemain = remain
		} else if remain > 0 && endsAt > 0 && endsAt == expiresAt {
			expiringRemain += remain
		}
	}
	sort.SliceStable(info.Packages, func(i, j int) bool {
		return info.Packages[i].EndsAt < info.Packages[j].EndsAt
	})
	if info.Total > 0 {
		info.Percentage = info.Used / info.Total * 100
	}
	info.Exceeded = info.Remaining <= 0
	info.ExpiresAt = expiresAt
	info.ExpiringRemain = expiringRemain

	window := providers.QuotaWindow{
		ID:         "credit",
		Label:      "MiniMax 积分",
		Used:       info.Used,
		Total:      info.Total,
		Remaining:  info.Remaining,
		Percentage: info.Percentage,
		Unit:       creditUnit,
		Exceeded:   info.Exceeded,
	}
	if expiresAt > 0 {
		window.ResetAt = time.Unix(expiresAt, 0).UTC().Format(time.RFC3339)
	}
	info.Windows = []providers.QuotaWindow{window}
	return info, nil
}
