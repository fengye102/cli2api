package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Quota reads the credit wallet and subscription info.
//
// AutoClaw is a pure credit system with no total cap: the wallet reports a
// remaining balance in "积分", not a percentage of a fixed pool. Reporting a
// fabricated percentage would make routing think an account is nearly spent
// when it is not, so the balance is surfaced as an absolute value and
// Exceeded is set only when the balance is genuinely zero.
func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	balance, err := c.queryBalance(ctx, accountID, cred)
	if err != nil {
		return nil, err
	}
	info := providers.QuotaInfo{
		Unit:      "积分",
		Remaining: balance,
		// Total is unknown by design: credits have no ceiling. Leaving it zero
		// keeps the console from drawing a 0% bar out of nowhere.
		Total: 0,
	}
	if balance <= 0 {
		info.Exceeded = true
	}
	return &info, nil
}

// queryBalance calls the wallet endpoint.
func (c *Client) queryBalance(ctx context.Context, accountID string, cred Credential) (float64, error) {
	requestCtx, cancel := context.WithTimeout(ctx, metadataTimeoutSeconds*time.Second)
	defer cancel()
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return 0, err
	}
	endpoint := strings.TrimRight(c.userapiBase, "/") + balancePath
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	for name, value := range signedHeaders(cred.AccessToken, c.now()) {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, c.transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, newProviderError(resp.StatusCode, "")
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return 0, fmt.Errorf("parse autoclaw balance: %w", err)
	}
	if envelope.Code != 0 {
		return 0, fmt.Errorf("autoclaw balance rejected: code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	return parseBalance(envelope.Data), nil
}

// parseBalance digs the total out of the wallet payload, which nests wallets
// and may spell the field several ways.
func parseBalance(raw json.RawMessage) float64 {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}
	for _, key := range []string{"total_balance", "totalBalance", "balance", "total", "points", "credit"} {
		if value, ok := numberFrom(payload[key]); ok {
			return value
		}
	}
	// Nested wallets: sum the balances, which is what "total" means here.
	if wallets, ok := payload["wallets"].([]any); ok {
		var total float64
		for _, item := range wallets {
			wallet, ok := item.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"balance", "amount", "remaining", "points"} {
				if value, ok := numberFrom(wallet[key]); ok {
					total += value
					break
				}
			}
		}
		return total
	}
	return 0
}

func numberFrom(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed, true
		}
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%f", &parsed); err == nil {
			return parsed, true
		}
	}
	return 0, false
}
