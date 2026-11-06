package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Credits live on the model gateway, not the account service:
//
//	GET {modelBase}/points/records?record_type=all&page_no=1&page_size=1
//	→ {"data":{"balance":9989,"dailyBalance":4989,"availableBalance":9989, …}}
//
// `availableBalance` is what a request may spend now (the daily budget and the
// account balance folded together), so it is the routing number. `balance` is
// the account total and `dailyBalance` the remaining allowance for today; both
// are shown as separate windows because an operator watching a 5000/day cap
// needs to see that number, not only the sum.

// pointsSnapshot is the subset of the records payload this adapter reads.
type pointsSnapshot struct {
	Balance      *float64 `json:"balance"`
	DailyBalance *float64 `json:"dailyBalance"`
	Available    *float64 `json:"availableBalance"`
}

// pointsRecords reads the credit balances, logging in again once if the
// session turns out to be dead (as a 401 or as the 100002 business envelope).
func (c *Client) pointsRecords(ctx context.Context, accountID string) (pointsSnapshot, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return pointsSnapshot{}, err
	}
	data, err := c.getPoints(ctx, accountID, cred.Session)
	if err != nil {
		if !isAuthError(err) {
			return pointsSnapshot{}, err
		}
		refreshed, reloginErr := c.relogin(ctx, accountID)
		if reloginErr != nil {
			return pointsSnapshot{}, reloginErr
		}
		if data, err = c.getPoints(ctx, accountID, refreshed.Session); err != nil {
			return pointsSnapshot{}, err
		}
	}
	var payload struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return pointsSnapshot{}, fmt.Errorf("loomy 积分响应无法解析：%w", err)
	}
	read := func(keys ...string) *float64 {
		for _, key := range keys {
			if value, ok := numberValue(payload.Data[key]); ok {
				return value
			}
		}
		return nil
	}
	floor := func(value *float64) *float64 {
		if value == nil {
			return nil
		}
		floored := math.Floor(*value)
		return &floored
	}
	return pointsSnapshot{
		Balance:      floor(read("balance")),
		DailyBalance: floor(read("dailyBalance", "daily_balance")),
		Available:    floor(read("availableBalance", "available_balance")),
	}, nil
}

func (c *Client) getPoints(ctx context.Context, accountID, session string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()

	endpoint := strings.TrimRight(c.modelBase, "/") + pointsRecordsPath +
		"?record_type=" + pointsQueryAllTypes + "&page_no=" + pointsFirstPage + "&page_size=" + quotaPointsPageSize
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range modelHeaders(session) {
		req.Header.Set(key, value)
	}
	client, err := c.modelClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &providers.Error{Kind: "unavailable", Message: "loomy 积分查询不可达：" + err.Error()}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, &providers.Error{Kind: "auth", Status: resp.StatusCode, Message: "loomy 积分查询：session 已失效"}
	}
	return readGatewayJSON(resp)
}

// Quota reports the credit balances.
func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	snapshot, err := c.pointsRecords(ctx, accountID)
	if err != nil {
		return nil, err
	}
	available, hasAvailable := snapshot.Available, snapshot.Available != nil
	if !hasAvailable {
		// Older gateway builds only report balance; treat it as the usable
		// number rather than inventing a zero.
		available, hasAvailable = snapshot.Balance, snapshot.Balance != nil
	}
	if !hasAvailable {
		return nil, fmt.Errorf("loomy 积分响应缺少可用余额")
	}

	windows := []providers.QuotaWindow{{
		ID: "available", Label: "可用积分", Unit: quotaUnit,
		Total: *available, Remaining: *available,
	}}
	if snapshot.Balance != nil {
		windows = append(windows, providers.QuotaWindow{
			ID: "balance", Label: "账户余额", Unit: quotaUnit,
			Total: *snapshot.Balance, Remaining: *snapshot.Balance,
		})
	}
	if snapshot.DailyBalance != nil {
		windows = append(windows, providers.QuotaWindow{
			ID: "daily", Label: "今日剩余", Unit: quotaUnit,
			Total: *snapshot.DailyBalance, Remaining: *snapshot.DailyBalance,
		})
	}

	return &providers.QuotaInfo{
		// The gateway reports what is left, with no granted total to divide
		// by, so Used/Percentage stay zero instead of inventing a denominator.
		Total:      *available,
		Remaining:  *available,
		Unit:       quotaUnit,
		Exceeded:   *available <= 0,
		FetchedAt:  time.Now().UTC().Format(time.RFC3339),
		Windows:    windows,
		ProviderID: "loomy",
	}, nil
}

// numberValue reads a JSON number that may arrive as a number or a string.
func numberValue(raw json.RawMessage) (*float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if value, err := number.Float64(); err == nil && !math.IsNaN(value) {
			return &value, true
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if value, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil && !math.IsNaN(value) {
			return &value, true
		}
	}
	return nil, false
}
