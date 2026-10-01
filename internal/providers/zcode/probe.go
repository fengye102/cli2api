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

// balanceURL is the ZCode plan-gateway billing endpoint. Live probes verified
// it returns the plan balance for an authorized Bearer JWT; the payload is
// best-effort and may be WAF-blocked on some paths, so failures degrade to a
// liveness-only probe.
const balanceURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"

// probeTimeout keeps liveness probes short so the executor can move on
// quickly when an account is slow.
const probeTimeout = 10 * time.Second

// Refresh re-checks the account credential by calling the plan-gateway
// balance endpoint. OAuth/JWT credentials use Bearer <zcode_jwt>; API-key
// credentials send x-api-key. A 401 (or 403) marks the credential expired so
// the manager classifies the account as auth-failed and the console surfaces
// re-login.
//
// Refresh never mints a new token: zcode OAuth refresh tokens are rotated by
// the official desktop client, and we deliberately do not replicate that
// dance here. The probe only validates the stored token still works.
func (c *Client) Refresh(ctx context.Context, accountID string, cred Credential) (Credential, error) {
	if !cred.hasUsableCredential() {
		return cred, fmt.Errorf("zcode credential missing chat credential; re-import required")
	}
	body, status, err := c.getBalance(ctx, accountID, cred)
	if err != nil {
		return cred, err
	}
	classified := Classify(status, string(body))
	if classified.Kind == accounts.KindAuth {
		uid := firstNonEmptyString(cred.Email, cred.UserID)
		_ = c.store.Observe(ctx, accountID, uid, "login_required",
			"zcode token rejected; re-import or re-login required", accounts.KindAuth)
		return cred, fmt.Errorf("zcode session dead: re-login required")
	}
	if status >= 300 {
		return cred, fmt.Errorf("zcode refresh status=%d: %s", status, strings.TrimSpace(string(body)))
	}
	return cred, nil
}

// Probe reports readiness for the account: a cheap authenticated probe
// against the billing/balance endpoint for oauth, or the same endpoint with
// x-api-key for api_key mode. Quota parse failures do not flip Ready; a 401
// always flips Ready=false via Refresh's Observe call.
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

// Quota fetches the plan balance for the account. The billing/balance payload
// shape observed live:
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
	body, status, err := c.getBalance(ctx, accountID, cred)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("zcode balance status=%d", status)
	}
	return parseBalanceQuota(body, cred), nil
}

// getBalance issues the GET against the balance endpoint with the right
// auth headers per credential mode. The endpoint takes app_version as a
// query parameter; the pinned ZCode client version goes there.
func (c *Client) getBalance(ctx context.Context, accountID string, cred Credential) ([]byte, int, error) {
	_ = accountID
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	url := balanceURL + "?app_version=" + Version
	if strings.TrimSpace(c.catalogURL) != "" {
		// Tests point the catalog at a local server; reuse the same override
		// for the balance endpoint so a single httptest server can serve both.
		url = strings.TrimRight(c.catalogURL, "/") + "/api/v1/zcode-plan/billing/balance?app_version=" + Version
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
				return nil, resp.StatusCode, fmt.Errorf("zcode balance response too large")
			}
		}
		if err != nil {
			break
		}
	}
	return body, resp.StatusCode, nil
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
