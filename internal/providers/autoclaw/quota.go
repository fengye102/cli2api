package autoclaw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	wallet, err := c.queryBalance(ctx, accountID, cred)
	if err != nil {
		return nil, err
	}
	info := providers.QuotaInfo{
		Unit:      "积分",
		Remaining: wallet.Total,
		// Total is unknown by design: credits have no ceiling. Leaving it zero
		// keeps the console from drawing a 0% bar out of nowhere.
		Total: 0,
		// One window per non-empty wallet, so the card shows where the credits
		// actually sit (奖励积分 / 每日积分 / 订阅积分) instead of a single
		// unexplained number. Without windows the runtime treats the snapshot
		// as "not fetched yet" and drops it entirely.
		Windows: wallet.Windows(),
	}
	if wallet.Total <= 0 {
		info.Exceeded = true
	}
	return &info, nil
}

// walletBalance is the parsed wallet view: the total plus the named buckets.
type walletBalance struct {
	Total   float64
	Buckets []walletBucket
}

// walletBucket is one named credit bucket (奖励积分 / 每日积分 / 订阅积分).
type walletBucket struct {
	Name    string
	Balance float64
}

// Windows turns the named buckets into quota windows. Buckets the upstream
// hides (display=false) or that hold nothing are dropped: a row of zeros tells
// the user nothing, and the total already covers the sum.
func (w walletBalance) Windows() []providers.QuotaWindow {
	out := make([]providers.QuotaWindow, 0, len(w.Buckets))
	for _, bucket := range w.Buckets {
		if bucket.Balance <= 0 {
			continue
		}
		out = append(out, providers.QuotaWindow{
			ID:        bucket.Name,
			Label:     bucket.Name,
			Remaining: bucket.Balance,
			Unit:      "积分",
			// Credits have no ceiling, so no percentage is published.
			Percentage: 0,
		})
	}
	return out
}

// queryBalance calls the wallet endpoint.
func (c *Client) queryBalance(ctx context.Context, accountID string, cred Credential) (walletBalance, error) {
	requestCtx, cancel := context.WithTimeout(ctx, metadataTimeoutSeconds*time.Second)
	defer cancel()
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return walletBalance{}, err
	}
	endpoint := strings.TrimRight(c.userapiBase, "/") + balancePath + "?biz_app_id=" + balanceAppID
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return walletBalance{}, err
	}
	for name, value := range signedHeaders(cred.AccessToken, c.now()) {
		req.Header.Set(name, value)
	}
	// The asset-manager service sits behind a WAF that fingerprints the
	// client, not just the signature: without a browser-shaped Origin/Referer
	// plus a trace id it answers 403/404 even for a valid token.
	req.Header.Set("X-Trace-Id", traceID())
	req.Header.Set("Origin", desktopOrigin)
	req.Header.Set("Referer", desktopOrigin+"/")
	resp, err := client.Do(req)
	if err != nil {
		return walletBalance{}, c.transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return walletBalance{}, newProviderError(resp.StatusCode, "")
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return walletBalance{}, fmt.Errorf("parse autoclaw balance: %w", err)
	}
	// A business-level 404 arrives with HTTP 200. Treating it as success is
	// what silently hid the real wallet: the caller then reads a zero balance
	// and renders "no quota" instead of surfacing a broken endpoint.
	if envelope.Code != 0 {
		return walletBalance{}, fmt.Errorf("autoclaw balance rejected: code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	return parseBalance(envelope.Data), nil
}

// desktopOrigin is the loopback origin the AutoClaw desktop client presents.
const desktopOrigin = "http://localhost:18432"

// traceID mints the 16-hex-char request trace the asset-manager WAF expects.
func traceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// parseBalance digs the total and the named buckets out of the wallet payload.
//
// The live shape (verified 2026-10-30) is:
//
//	{"total_balance":13968,
//	 "wallets":[{"public_wallet_type":"reward","display_name":"奖励积分",
//	             "balance":13968,"display":true}, ...]}
//
// Older spellings are still accepted so a payload change does not blank the
// card again.
func parseBalance(raw json.RawMessage) walletBalance {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return walletBalance{}
	}
	var out walletBalance
	for _, key := range []string{"total_balance", "totalBalance", "balance", "total", "points", "credit"} {
		if value, ok := numberFrom(payload[key]); ok {
			out.Total = value
			break
		}
	}
	if wallets, ok := payload["wallets"].([]any); ok {
		for _, item := range wallets {
			bucket, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// display=false buckets are hidden by the upstream itself.
			if hidden, ok := bucket["display"].(bool); ok && !hidden {
				continue
			}
			var amount float64
			var found bool
			for _, key := range []string{"balance", "amount", "remaining", "points"} {
				if value, ok := numberFrom(bucket[key]); ok {
					amount, found = value, true
					break
				}
			}
			if !found {
				continue
			}
			name := "积分"
			for _, key := range []string{"display_name", "public_wallet_type", "name"} {
				if value, ok := bucket[key].(string); ok && strings.TrimSpace(value) != "" {
					name = strings.TrimSpace(value)
					break
				}
			}
			out.Buckets = append(out.Buckets, walletBucket{Name: name, Balance: amount})
		}
	}
	// No explicit total: the sum of the buckets is what "total" means here.
	if out.Total == 0 {
		for _, bucket := range out.Buckets {
			out.Total += bucket.Balance
		}
	}
	return out
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
