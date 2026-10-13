package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Claim implements the ZCode Start-Plan (活动套餐) claim flow ported from
// agent2api's zcode/claim.rs. The plan gateway exposes two billing endpoints:
//
//	GET  {gateway}/billing/preview?app_version=<v>&platform=<p>
//	     -> {"code":0,"data":{"plans":[{plan_id,name,starts_at,ends_at,...}]}}
//	POST {gateway}/billing/claim  body {"plan_id":"..."}
//	     -> {"code":0,"data":{"plan":{...}}}  or  {"code":1003,"msg":"already claimed"}
//
// preview may be called anonymously (no Authorization header); claim requires
// the account's ZCode JWT and, when upstream's captcha config demands it, an
// Aliyun traceless-verification token. Both endpoints honour the same
// X-Device-Mid the chat path sends (the upstream ties these calls to a device).

const (
	// claimPreviewPath is the billing preview path under the plan gateway.
	claimPreviewPath = "/billing/preview"
	// claimPath is the billing claim path under the plan gateway.
	claimPath = "/billing/claim"
)

// The provider-facing claim types live in internal/providers (claim.go):
// providers.ClaimPlan and providers.ClaimResult. This file implements the
// account-level preview and claim calls against the plan gateway.
// PreviewClaim lists the claimable Start Plans for the account. When
// credential JWT is empty the request is anonymous (upstream allows preview
// without auth) — callers usually have an OAuth credential, so this path
// mostly runs authenticated.
func (c *Client) PreviewClaim(ctx context.Context, accountID string) ([]providers.ClaimPlan, error) {
	cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	path := claimPreviewPath
	query := url.Values{}
	query.Set("app_version", Version)
	query.Set("platform", platformID())
	endpoint := gatewayBaseURL + path + "?" + query.Encode()
	if strings.TrimSpace(c.catalogURL) != "" {
		endpoint = strings.TrimRight(c.catalogURL, "/") + gatewayPathPrefix + path + "?" + query.Encode()
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range defaultHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	if token := zcodeJWT(cred); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if mid := deviceMid(accountID); mid != "" {
		req.Header.Set("X-Device-Mid", mid)
	}
	body, status, err := doRaw(c.http, req)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status == http.StatusUnauthorized {
		return nil, fmt.Errorf("zcode session dead: re-login required")
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Plans []providers.ClaimPlan `json:"plans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("zcode claim preview: malformed response (status=%d)", status)
	}
	if status < 200 || status >= 300 || payload.Code != 0 {
		return nil, fmt.Errorf("zcode claim preview failed (status=%d code=%d): %s",
			status, payload.Code, strings.TrimSpace(string(body)))
	}
	return payload.Data.Plans, nil
}

// Claim claims the plan with the given planID. captchaParam carries the Aliyun
// traceless-verification token minted by the captcha node (empty when upstream
// config does not demand one). A successful claim returns ClaimResult.Claimed;
// the already-claimed business error (code 1003) is surfaced as
// ClaimResult.AlreadyClaimed rather than a hard error so callers can render
// "claimed today" without treating it as failure.
func (c *Client) Claim(ctx context.Context, accountID, planID string) (providers.ClaimResult, error) {
	cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return providers.ClaimResult{}, err
	}
	token := zcodeJWT(cred)
	if token == "" {
		return providers.ClaimResult{}, fmt.Errorf("zcode claim needs a logged-in OAuth credential")
	}
	if strings.TrimSpace(planID) == "" {
		return providers.ClaimResult{}, fmt.Errorf("zcode claim needs a plan_id")
	}

	// The plan channel requires a captcha token on every OAuth request (see
	// captcha.go). Mint one when the node is available; upstream answers 3007
	// when it wants a token and does not get one, and answers 3007 too when it
	// gets a token it does not want — so only send the header when we have one.
	captchaParam := ""
	if c.captcha != nil && c.captcha.enabled() {
		param, _, err := c.captcha.param(ctx)
		if err == nil {
			captchaParam = param
		}
	}

	endpoint := gatewayBaseURL + claimPath
	if strings.TrimSpace(c.catalogURL) != "" {
		endpoint = strings.TrimRight(c.catalogURL, "/") + gatewayPathPrefix + claimPath
	}
	body, _ := json.Marshal(map[string]string{"plan_id": planID})

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.ClaimResult{}, err
	}
	for k, v := range defaultHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if captchaParam != "" {
		req.Header.Set(CaptchaHeader, captchaParam)
	}
	if mid := deviceMid(accountID); mid != "" {
		req.Header.Set("X-Device-Mid", mid)
	}

	respBody, status, err := doRaw(c.http, req)
	if err != nil {
		return providers.ClaimResult{}, err
	}
	if status == http.StatusUnauthorized {
		return providers.ClaimResult{}, fmt.Errorf("zcode session dead: re-login required")
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Plan struct {
				PlanID string `json:"plan_id"`
				Name   string `json:"name"`
			} `json:"plan"`
		} `json:"data"`
	}
	_ = json.Unmarshal(respBody, &payload)

	// Business outcomes:
	//   0    -> claimed
	//   1003 -> already claimed today (not an error)
	//   3007 -> captcha failed / required
	//   other -> hard failure
	switch {
	case status >= 200 && status < 300 && payload.Code == 0:
		return providers.ClaimResult{Claimed: true, PlanID: planID, Message: "claimed"}, nil
	case payload.Code == 1003 || strings.Contains(strings.ToLower(payload.Msg), "already claimed"):
		return providers.ClaimResult{AlreadyClaimed: true, PlanID: planID, Message: firstNonEmptyString(payload.Msg, "already claimed")}, nil
	case payload.Code == 3007 || strings.Contains(strings.ToLower(payload.Msg), "captcha"):
		return providers.ClaimResult{}, fmt.Errorf("zcode claim captcha failed: %s", firstNonEmptyString(payload.Msg, "captcha required"))
	default:
		return providers.ClaimResult{}, fmt.Errorf("zcode claim failed (status=%d code=%d): %s",
			status, payload.Code, firstNonEmptyString(payload.Msg, strings.TrimSpace(string(respBody))))
	}
}

// zcodeJWT returns the account's chat credential: the ZCode JWT when an OAuth
// credential has one, else the access token.
func zcodeJWT(cred Credential) string {
	if v := strings.TrimSpace(cred.ZCodeJWT); v != "" {
		return v
	}
	return strings.TrimSpace(cred.AccessToken)
}

// doRaw performs a request and reads the full (capped) response body, sharing
// the gateway helper's cap so a hostile response cannot balloon memory.
func doRaw(client *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 1<<20 {
				return nil, resp.StatusCode, fmt.Errorf("zcode gateway response too large")
			}
		}
		if err != nil {
			break
		}
	}
	return buf, resp.StatusCode, nil
}
