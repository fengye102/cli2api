package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// The plan gateway carries every authenticated ZCode account call that is not
// a chat completion: the plans/billing views live here, and the app_version
// query parameter is mandatory (an unknown build is answered with
// {"code":3001,"msg":"parameter error"}).
const (
	gatewayBaseURL = "https://zcode.z.ai/api/v1/zcode-plan"
	// gatewayPathPrefix mirrors gatewayBaseURL's path: the test override
	// swaps the host only.
	gatewayPathPrefix = "/api/v1/zcode-plan"

	// plansPath is the endpoint that reports what the account is entitled
	// to. Live-verified for an OAuth credential (2026-10-02):
	//
	//	GET .../billing/current?app_version=<v>  Authorization: Bearer <jwt>
	//	-> 200 {"code":0,"data":{"plans":[{"plan_id":"zcode-v3-start-plan-trust-1002",
	//	     "name":"ZCode Trust Build","status":"active","ends_at":...,
	//	     "entitlements":[{"show_name":"GLM-5.3-Flash","unit_type":"token",
	//	     "grant_units":100000000,...}]}]}}
	//
	// while balancePath answers it with 400 {"code":3001,"msg":"parameter
	// error"}. Probing balancePath therefore reported a healthy, freshly
	// signed-in Start-Plan account as "登录失败 / 额度不可用"; plansPath is the
	// endpoint that reflects the account's real state.
	plansPath = "/billing/current"

	// balancePath is the legacy remaining-balance view. It only answers for
	// funded (pay-as-you-go) credentials, and it is the probe for API keys.
	balancePath = "/billing/balance"
)

// probeTimeout keeps liveness probes short so the executor can move on
// quickly when an account is slow.
const probeTimeout = 10 * time.Second

// Refresh re-checks the account credential against the plan gateway: the
// plans endpoint for OAuth/JWT credentials, the balance endpoint for API
// keys. A 401 (or 403) marks the credential expired so the manager classifies
// the account as auth-failed and the console surfaces re-login.
//
// Any other non-2xx answer means the gateway accepted the credential but has
// nothing to report for it (a Start-Plan account on the balance endpoint, for
// instance), so the account stays usable instead of being flipped to error. A
// 429/5xx is still surfaced: that is upstream trouble, not an account state.
//
// Refresh never mints a new token: zcode OAuth refresh tokens are rotated by
// the official desktop client, and we deliberately do not replicate that
// dance here. The probe only validates the stored token still works.
func (c *Client) Refresh(ctx context.Context, accountID string, cred Credential) (Credential, error) {
	if !cred.hasUsableCredential() {
		return cred, fmt.Errorf("zcode credential missing chat credential; re-import required")
	}
	body, status, err := c.probe(ctx, accountID, cred)
	if err != nil {
		return cred, err
	}
	if Classify(status, string(body)).Kind == accounts.KindAuth {
		uid := firstNonEmptyString(cred.Email, cred.UserID)
		_ = c.store.Observe(ctx, accountID, uid, "login_required",
			"zcode token rejected; re-import or re-login required", accounts.KindAuth)
		return cred, fmt.Errorf("zcode session dead: re-login required")
	}
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
		return cred, fmt.Errorf("zcode refresh status=%d: %s", status, strings.TrimSpace(string(body)))
	}
	return cred, nil
}

// probe issues the readiness call that matches the credential's auth mode.
func (c *Client) probe(ctx context.Context, accountID string, cred Credential) ([]byte, int, error) {
	if cred.IsOAuth() {
		return c.gatewayGet(ctx, accountID, cred, plansPath)
	}
	return c.gatewayGet(ctx, accountID, cred, balancePath)
}

// Probe reports readiness for the account: a cheap authenticated probe
// against the plans (oauth) or balance (api_key) endpoint. Quota parse
// failures do not flip Ready; a 401 always flips Ready=false via Refresh's
// Observe call.
func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	if !cred.Ready() {
		return providers.AccountHealth{UID: cred.UserID,
			LastError: "zcode credential incomplete; re-import required"}, nil
	}
	if _, err := c.Refresh(ctx, accountID, cred); err != nil {
		return providers.AccountHealth{
			UID:       firstNonEmptyString(cred.Email, cred.UserID),
			LastError: err.Error(),
		}, nil
	}
	return providers.AccountHealth{
		Ready: true,
		Hot:   true,
		UID:   firstNonEmptyString(cred.Email, cred.UserID),
	}, nil
}

// Quota fetches the plan state for the account. OAuth credentials read the
// plans endpoint (plan name + granted entitlement units); API keys keep the
// balance payload:
//
//	{"code":0,"data":{"balance":123.45,"currency":"CNY","plan":"pro"}}
//
// All numeric fields are best-effort; missing or unparseable values produce a
// nil info (never a hard error), so a probe failure does not flip Ready.
func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !cred.Ready() {
		return nil, fmt.Errorf("zcode credential incomplete; re-import required")
	}
	path := plansPath
	if !cred.IsOAuth() {
		path = balancePath
	}
	body, status, err := c.gatewayGet(ctx, accountID, cred, path)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("zcode quota status=%d", status)
	}
	if cred.IsOAuth() {
		if info := parsePlansQuota(body, cred); info != nil {
			return info, nil
		}
		// A plan payload without a plan is still a successful probe; fall
		// through to the balance shape so an older gateway still renders.
	}
	return parseBalanceQuota(body, cred), nil
}

// gatewayGet issues an authenticated GET against a plan-gateway path. The
// endpoint takes app_version as a query parameter; the pinned ZCode client
// version goes there.
func (c *Client) gatewayGet(ctx context.Context, accountID string, cred Credential, path string) ([]byte, int, error) {
	_ = accountID
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if strings.TrimSpace(path) == "" {
		path = balancePath
	}
	url := gatewayBaseURL + path + "?app_version=" + Version
	if strings.TrimSpace(c.catalogURL) != "" {
		// Tests point the catalog at a local server; reuse the same override
		// so a single httptest server can serve every gateway path.
		url = strings.TrimRight(c.catalogURL, "/") + gatewayPathPrefix + path + "?app_version=" + Version
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range defaultHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	if cred.IsOAuth() {
		token := strings.TrimSpace(cred.ZCodeJWT)
		if token == "" {
			token = strings.TrimSpace(cred.AccessToken)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("x-api-key", token)
		}
	} else if key := strings.TrimSpace(cred.APIKey); key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
			if len(body) > 1<<20 {
				return nil, resp.StatusCode, fmt.Errorf("zcode gateway response too large")
			}
		}
		if err != nil {
			break
		}
	}
	return body, resp.StatusCode, nil
}

// planEntry is one plan of the gateway's plans payload.
type planEntry struct {
	PlanID       string            `json:"plan_id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	Priority     int               `json:"priority"`
	StartsAt     int64             `json:"starts_at"`
	EndsAt       int64             `json:"ends_at"`
	Entitlements []planEntitlement `json:"entitlements"`
}

// planEntitlement is one granted entitlement inside a plan.
type planEntitlement struct {
	EntitlementID string   `json:"entitlement_id"`
	ShowName      string   `json:"show_name"`
	Meter         string   `json:"meter"`
	UnitType      string   `json:"unit_type"`
	Period        string   `json:"period"`
	GrantUnits    *float64 `json:"grant_units"`
	Capabilities  []string `json:"capabilities"`
}

// parsePlansQuota converts the billing/current payload into QuotaInfo: the
// served plan plus one window per entitlement. The payload reports the units
// granted (grant_units) but not the units consumed, so usage stays at zero and
// only the granted total is published — inventing a used value would put a
// fake percentage on the console card.
func parsePlansQuota(body []byte, cred Credential) *providers.QuotaInfo {
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Plans []planEntry `json:"plans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	plan := pickActivePlan(payload.Data.Plans)
	if plan == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	unit := ""
	resetAt := ""
	if plan.EndsAt > 0 {
		resetAt = time.Unix(plan.EndsAt, 0).UTC().Format(time.RFC3339)
	}
	windows := make([]providers.QuotaWindow, 0, len(plan.Entitlements))
	var total float64
	for _, ent := range plan.Entitlements {
		if ent.UnitType != "" {
			unit = ent.UnitType + "s"
		}
		granted := 0.0
		if ent.GrantUnits != nil {
			granted = *ent.GrantUnits
		}
		total += granted
		windows = append(windows, providers.QuotaWindow{
			ID:        firstNonEmptyString(ent.EntitlementID, ent.ShowName, plan.PlanID),
			Label:     firstNonEmptyString(ent.ShowName, plan.Name, plan.PlanID),
			Total:     granted,
			Remaining: granted,
			Unit:      unit,
			ResetAt:   resetAt,
		})
	}
	if unit == "" {
		unit = "units"
	}
	if len(windows) == 0 {
		windows = append(windows, providers.QuotaWindow{
			ID:      firstNonEmptyString(plan.PlanID, "plan"),
			Label:   firstNonEmptyString(plan.Name, plan.PlanID),
			Unit:    unit,
			ResetAt: resetAt,
		})
	}
	info := &providers.QuotaInfo{
		FetchedAt:  now,
		Unit:       unit,
		ProviderID: "zcode",
		Plan:       strings.TrimSpace(firstNonEmptyString(plan.Name, plan.PlanID, cred.Plan)),
		Total:      total,
		Remaining:  total,
		Windows:    windows,
	}
	if plan.EndsAt > 0 {
		info.ExpiresAt = plan.EndsAt
		info.ExpiringRemain = total
	}
	return info
}

// pickActivePlan returns the plan the account is served from: the first
// active entry, else the highest-priority one (the gateway lists plans in
// priority order, preferring a paid Coding Plan over the Start Plan).
func pickActivePlan(plans []planEntry) *planEntry {
	if len(plans) == 0 {
		return nil
	}
	best := &plans[0]
	for i := range plans {
		if strings.EqualFold(strings.TrimSpace(plans[i].Status), "active") {
			return &plans[i]
		}
		if plans[i].Priority > best.Priority {
			best = &plans[i]
		}
	}
	return best
}

// parseBalanceQuota converts the billing/balance payload into QuotaInfo. The
// balance is a remaining amount; we synthesize a single "balance" window so
// the console card renders even without a used/total pair.
func parseBalanceQuota(body []byte, cred Credential) *providers.QuotaInfo {
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Balance  *float64 `json:"balance"`
			Currency string   `json:"currency"`
			Plan     string   `json:"plan"`
			Used     *float64 `json:"used"`
			Total    *float64 `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	if payload.Data.Balance == nil && payload.Data.Used == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	unit := strings.TrimSpace(payload.Data.Currency)
	if unit == "" {
		unit = "credits"
	}
	info := &providers.QuotaInfo{
		FetchedAt:  now,
		Unit:       unit,
		ProviderID: "zcode",
		Plan:       strings.TrimSpace(firstNonEmptyString(payload.Data.Plan, cred.Plan)),
	}
	if payload.Data.Total != nil && *payload.Data.Total > 0 {
		info.Total = *payload.Data.Total
	}
	if payload.Data.Used != nil {
		info.Used = *payload.Data.Used
	}
	if payload.Data.Balance != nil {
		info.Remaining = *payload.Data.Balance
		if info.Total <= 0 {
			// Synthesize a total so percentage is meaningful when only
			// remaining is reported.
			info.Total = info.Used + info.Remaining
		}
	} else if info.Total > 0 {
		info.Remaining = info.Total - info.Used
	}
	if info.Total > 0 {
		info.Percentage = 100 * info.Used / info.Total
	}
	info.Exceeded = info.Remaining <= 0 && info.Total > 0
	window := providers.QuotaWindow{
		ID:         "balance",
		Label:      "plan balance",
		Used:       info.Used,
		Total:      info.Total,
		Remaining:  info.Remaining,
		Percentage: info.Percentage,
		Unit:       unit,
		Exceeded:   info.Exceeded,
	}
	info.Windows = []providers.QuotaWindow{window}
	return info
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
