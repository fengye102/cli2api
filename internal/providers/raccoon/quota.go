package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Raccoon's credit balance and subscription both live on the main site, not on
// the LLM gateway:
//
//	GET {main}/api/web/points/v1/balance        ← the credit balance (authority)
//	GET {main}/api/web/auth/v1/entitlement_info ← the subscription (best effort)
//
// The balance response's `data` carries `available_points` (the usable total)
// plus four wallet buckets (daily / monthly membership / reward / top-up). The
// subscription carries two entitlement sets (office / code).
//
// Both calls are a bare fetch in the reference implementation: the account-level
// proxy exists for the long streaming chat requests, and the main site is a
// different host from the LLM gateway. They therefore go out directly on
// c.http, exactly like the check-in chain (see checkin.go).
//
// ── Why the subscription never fails the reading ────────────────────────
// The subscription is additional information. Turning a good balance into a
// "query failed" card because entitlement_info hiccuped is strictly worse than
// showing the balance without a plan name, so a subscription error is swallowed
// and only the balance decides the outcome.
//
// ── Why a zero balance does not set Exceeded ────────────────────────────
// Exceeded drives routing exclusion (pool.MergeQuota → item.Quota.Exceeded).
// Upstream reports no exhaustion flag on this endpoint, and the catalog serves
// models whose effective multiplier is zero (free models), so an account at
// zero points is not necessarily unable to serve. The honest reading is the
// number itself; a genuine upstream quota error still arrives through the chat
// classifier and cools the account down there.

const (
	// balancePath is the credit balance endpoint under the main site.
	balancePath = "/api/web/points/v1/balance"
	// entitlementPath is the subscription entitlement endpoint under the auth
	// gateway, which shares the main site origin.
	entitlementPath = "/api/web/auth/v1/entitlement_info"

	// balanceTimeout / entitlementTimeout bound the two reads. The upstream
	// read timeout is long (reserved for streaming chat), so an explicit cap
	// keeps a hung balance request from stalling the console's quota refresh.
	balanceTimeout     = 20 * time.Second
	entitlementTimeout = 20 * time.Second

	// quotaUnit is Raccoon's accounting unit: credits ("积分").
	quotaUnit = "积分"
)

// walletBucket maps one balance field to its display label. The order is the
// order the upstream panel shows, so the console renders the same sequence.
type walletBucket struct {
	Key   string
	Label string
}

var walletBuckets = []walletBucket{
	{"daily_points", "每日积分"},
	{"monthly_points", "每月会员积分"},
	{"reward_points", "奖励积分"},
	{"topup_points", "充值积分"},
}

// Quota reads the account's credit balance and, best effort, its subscription.
// A subscription failure does not fail the reading; a balance failure does, so
// the caller keeps whatever it had instead of saving an invented zero.
func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	token, err := c.siteToken(ctx, accountID, credential)
	if err != nil {
		return nil, err
	}

	data, err := c.requestJSON(ctx, accountID, http.MethodGet, balancePath, token, nil, balanceTimeout)
	if errors.Is(err, errSessionExpired) {
		// A stored access token can simply be stale. Refresh once and retry,
		// exactly like chat and check-in: without this every account whose token
		// aged out would report a permanent quota failure.
		refreshed, refreshErr := c.refresh(ctx, accountID, credential)
		if refreshErr == nil && strings.TrimSpace(refreshed.AccessToken) != "" {
			token = strings.TrimSpace(refreshed.AccessToken)
			data, err = c.requestJSON(ctx, accountID, http.MethodGet, balancePath, token, nil, balanceTimeout)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("小浣熊额度：积分查询失败：%w", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("小浣熊额度：积分响应无法解析：%w", err)
	}
	available, ok := pointsValue(payload["available_points"])
	if !ok {
		return nil, fmt.Errorf("小浣熊额度：积分响应缺少 available_points")
	}

	// The available total is always reported as a window, even at zero: the
	// runtime treats a window-less snapshot as "not fetched yet" and refuses to
	// store it (see runtime.quotaInfoHasWindows), so a genuinely empty account
	// would otherwise keep showing the "no quota data" state forever instead of
	// an honest 0.
	windows := []providers.QuotaWindow{{
		ID:        "available",
		Label:     "可用积分",
		Total:     available,
		Remaining: available,
		Unit:      quotaUnit,
	}}
	for _, bucket := range walletBuckets {
		balance, found := pointsValue(payload[bucket.Key])
		if !found {
			continue
		}
		windows = append(windows, providers.QuotaWindow{
			ID:        bucket.Key,
			Label:     bucket.Label,
			Total:     balance,
			Remaining: balance,
			Unit:      quotaUnit,
		})
	}

	return &providers.QuotaInfo{
		// Upstream reports only what is left, with no granted total to divide
		// by, so Used/Percentage stay zero rather than inventing a denominator.
		Total:      available,
		Remaining:  available,
		Unit:       quotaUnit,
		FetchedAt:  nowString(),
		Windows:    windows,
		ProviderID: "raccoon",
		Plan:       c.subscriptionPlan(ctx, accountID, token),
	}, nil
}

// entitlementSet is one of the two subscription sets (office / code).
type entitlementSet struct {
	PlanType   string `json:"plan_type"`
	ActivePlan struct {
		Type string `json:"type"`
	} `json:"active_plan"`
}

func (e entitlementSet) planName() string {
	return firstNonEmpty(e.ActivePlan.Type, e.PlanType)
}

// subscriptionPlan reads the subscription tier, returning "" when the upstream
// does not report one or the call fails. Best effort by design: see the module
// comment on why a subscription miss must not fail the balance reading.
func (c *Client) subscriptionPlan(ctx context.Context, accountID, token string) string {
	data, err := c.requestJSON(ctx, accountID, http.MethodGet, entitlementPath, token, nil, entitlementTimeout)
	if err != nil {
		return ""
	}
	var payload struct {
		Office entitlementSet `json:"office"`
		Code   entitlementSet `json:"code"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	// Office is the desktop client's main entry, so it wins; code is the
	// fallback, matching the reference implementation's precedence.
	return firstNonEmpty(payload.Office.planName(), payload.Code.planName())
}

// pointsValue reads one credit field. Upstream occasionally writes numbers as
// strings, so both spellings resolve; anything else (missing, null, garbage) is
// reported as absent rather than silently becoming a zero balance.
func pointsValue(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if value, err := number.Float64(); err == nil && !math.IsNaN(value) {
			return math.Floor(value), true
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if value, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil && !math.IsNaN(value) {
			return math.Floor(value), true
		}
	}
	return 0, false
}

func nowString() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z07:00")
}
