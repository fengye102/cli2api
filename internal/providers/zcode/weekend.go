package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Weekend grab ported from angusdevgo/ZCode2api's weekend-grab loop (read
// 2026-10-31, /tmp/ZCode2api). Upstream runs promo (周末活动包) plans with a
// date-encoded plan_id — `zcode-v3-start-plan-MMDD-wk` where MMDD is the
// Beijing-time issue date — and opens the claim window Friday 18:00 (Beijing)
// through Sunday midnight. Claims inside the window are first-come: waiting
// for preview to list the plan loses the quota, so the grab probes candidate
// ids directly and treats business-code 1005 (名额未开/已尽) as retry-later,
// 1003 (already claimed) as done.
//
// The daily Start Plan (`-0817`) is NOT part of the weekend window and the
// manual claim flow already covers it; this sweep only chases the weekend
// promo so it stays quiet during the week.

const (
	// weekendPlanPrefix/weekendPlanSuffix frame the date-encoded promo id:
	// zcode-v3-start-plan-<MMDD>-wk (measured against live promo ids and the
	// reference implementation's candidate builder).
	weekendPlanPrefix = "zcode-v3-start-plan-"
	weekendPlanSuffix = "-wk"

	// weekendClaimCodes are the business codes billing/claim returns that
	// decide the sweep's next step:
	//   0    -> claimed
	//   1003 -> already claimed (the promo is on the account)
	//   1005 -> 名额未开/已尽 — the window exists upstream has no quota left
	//           for this round; retry on the next sweep tick.
	//   1001 -> unknown plan id (wrong date guess; try the next candidate)
	weekendClaimCodeClaimed = 0
	weekendClaimCodeDup     = 1003
	// weekendClaimCodeNotOpen covers both upstream refusal shapes: 1005
	// (名额未开/已尽) and 1004 (no permission — this account tier cannot
	// take the promo). Both mean "this round is not going to land, retry on
	// the next sweep tick or never"; neither is a hard error.
	weekendClaimCodeNotOpen  = 1005
	weekendClaimCodeNoPerm   = 1004
	weekendClaimCodeNoPlanID = 1001
)

// autoClaimResult is the compact outcome of one weekend-grab round for one
// account. It feeds both the maintenance log line and the account's
// LastCheckin* display fields (the console renders them without knowing about
// zcode specifics).
type autoClaimResult struct {
	Claimed     bool
	AlreadyHad  bool
	PlanID      string
	Code        int
	Message     string
	NeedRetry   bool
	Unavailable bool
}

// WeekendClaimOpen reports whether now falls inside the promo claim window in
// Beijing time: Friday from 18:00, all of Saturday, all of Sunday (the window
// closes when Monday 00:00 rolls over, which the day<5 branch covers).
func WeekendClaimOpen(now time.Time) bool {
	bj := now.In(beijingLocation())
	switch bj.Weekday() {
	case time.Friday:
		return bj.Hour() >= 18
	case time.Saturday, time.Sunday:
		return true
	default:
		return false
	}
}

// beijingLocation is fixed UTC+8. LoadLocation("Asia/Shanghai") would be the
// purist choice but embeds the tzdata dependency for a zone that has had no
// DST since 1991; the fixed offset is what the reference implementation does
// too (Date.now() + 8h on UTC getters).
func beijingLocation() *time.Location {
	return time.FixedZone("UTC+8", 8*3600)
}

// weekendPlanCandidates builds the promo plan ids to try for a claim round:
// the date-encoded id for today, yesterday and tomorrow in Beijing time. The
// ±1 day slack matters because the window spans a midnight: a claim at
// Saturday 23:58 lands on the Sunday-encoded id a minute later, and upstream
// has been observed issuing the next week's id before the window opens.
func weekendPlanCandidates(now time.Time) []string {
	bj := now.In(beijingLocation())
	day := 24 * time.Hour
	offsets := []time.Duration{0, -day, day}
	seen := map[string]bool{}
	out := make([]string, 0, len(offsets))
	for _, off := range offsets {
		d := bj.Add(off)
		id := weekendPlanPrefix + d.Format("0102") + weekendPlanSuffix
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// activeWeekendPlan reports whether the account's billing/balance plans list
// already contains an active promo (weekend) plan, so the sweep can skip the
// account without burning claim calls. Reuses the /billing/balance response
// the quota path already speaks.
func (c *Client) activeWeekendPlan(ctx context.Context, accountID string) (bool, error) {
	cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return false, err
	}
	body, status, err := c.gatewayGet(ctx, accountID, cred, balancePath)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("zcode balance status=%d", status)
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Plans []planEntry `json:"plans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, fmt.Errorf("zcode balance malformed: %w", err)
	}
	if payload.Code != 0 {
		return false, fmt.Errorf("zcode balance code=%d", payload.Code)
	}
	now := time.Now().Unix()
	for _, plan := range payload.Data.Plans {
		if plan.Status == "active" && plan.EndsAt > now && isWeekendPlanID(plan.PlanID) {
			return true, nil
		}
	}
	return false, nil
}

// isWeekendPlanID recognises the promo id shape regardless of the embedded
// date: `zcode-v3-start-plan-<MMDD>-wk`. The name is checked too because the
// reference implementation found upstream occasionally renames the id but
// keeps "weekend"/"wk" in the display name.
func isWeekendPlanID(planID string) bool {
	s := strings.ToLower(planID)
	if strings.HasPrefix(s, weekendPlanPrefix) && strings.HasSuffix(s, weekendPlanSuffix) {
		return true
	}
	return strings.Contains(s, "weekend")
}

// AutoClaimWeekend runs one weekend-grab round for the account: skip when a
// promo plan is already active, otherwise POST billing/claim for each
// candidate id until one lands (or the codes say stop trying this round).
// Claimed/AlreadyHad end the sweep; 1005 keeps NeedRetry set so the caller
// comes back on the next tick while the window is open.
func (c *Client) AutoClaimWeekend(ctx context.Context, accountID string) (providers.ClaimResult, error) {
	result := providers.ClaimResult{}
	active, err := c.activeWeekendPlan(ctx, accountID)
	if err != nil {
		return result, fmt.Errorf("zcode weekend check: %w", err)
	}
	if active {
		return providers.ClaimResult{AlreadyClaimed: true, Message: "weekend plan active"}, nil
	}

	// The plan channel demands a captcha token on OAuth writes (see Claim);
	// mint one when the node is available, silently go without when not —
	// upstream answers 3007 and the sweep just retries next tick.
	captchaParam := ""
	if c.captcha != nil && c.captcha.enabled() {
		if param, _, err := c.captcha.param(ctx); err == nil {
			captchaParam = param
		}
	}

	var last autoClaimResult
	for _, planID := range weekendPlanCandidates(time.Now()) {
		result, err := c.claimPlan(ctx, accountID, planID, captchaParam)
		if err != nil {
			return result, err
		}
		// claimPlan maps business codes into ClaimResult; recover the raw
		// code from the message is fragile, so the switch below re-derives
		// the decision from the fields it set.
		if result.Claimed {
			return result, nil
		}
		if result.AlreadyClaimed {
			return result, nil
		}
		last = autoClaimResult{PlanID: planID, Message: result.Message, NeedRetry: true}
		if result.ResetAt != "" {
			// claimPlan encodes the business code in ResetAt for the sweep;
			// decode it back (see claimPlan for the encoding).
			fmt.Sscanf(result.ResetAt, "code=%d", &last.Code)
		}
		switch last.Code {
		case weekendClaimCodeNotOpen, weekendClaimCodeNoPerm, weekendClaimCodeNoPlanID:
			// 名额未开/已尽、无资格或候选 id 不存在：下一个候选 id 仍可能
			// 命中，继续。
			continue
		case weekendClaimCodeDup:
			return providers.ClaimResult{AlreadyClaimed: true, PlanID: planID, Message: result.Message}, nil
		default:
			return result, nil
		}
	}
	_ = last
	return providers.ClaimResult{Message: "no weekend plan claimed this round", ResetAt: "code=1005"}, nil
}

// claimPlan is the raw billing/claim POST shared by the manual Claim flow and
// the weekend sweep. On business failure it returns a ClaimResult whose
// ResetAt field carries "code=<n>" so AutoClaimWeekend can branch on the raw
// business code without a second response type; Claim wraps the same helper
// for the operator-facing path.
func (c *Client) claimPlan(ctx context.Context, accountID, planID, captchaParam string) (providers.ClaimResult, error) {
	cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return providers.ClaimResult{}, err
	}
	token := zcodeJWT(cred)
	if token == "" {
		return providers.ClaimResult{}, fmt.Errorf("zcode claim needs a logged-in OAuth credential")
	}

	endpoint := gatewayBaseURL + claimPath
	if strings.TrimSpace(c.catalogURL) != "" {
		endpoint = strings.TrimRight(c.catalogURL, "/") + gatewayPathPrefix + claimPath
	}
	body, _ := json.Marshal(map[string]string{"plan_id": planID})

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
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

	switch {
	case status >= 200 && status < 300 && payload.Code == weekendClaimCodeClaimed:
		return providers.ClaimResult{Claimed: true, PlanID: planID, Message: firstNonEmptyString(payload.Msg, "claimed")}, nil
	case payload.Code == weekendClaimCodeDup || strings.Contains(strings.ToLower(payload.Msg), "already claimed"):
		return providers.ClaimResult{AlreadyClaimed: true, PlanID: planID, Message: firstNonEmptyString(payload.Msg, "already claimed")}, nil
	case payload.Code == 3007 || strings.Contains(strings.ToLower(payload.Msg), "captcha"):
		return providers.ClaimResult{}, fmt.Errorf("zcode claim captcha failed: %s", firstNonEmptyString(payload.Msg, "captcha required"))
	default:
		// Business failure with the raw code embedded for the sweep.
		return providers.ClaimResult{
			PlanID:  planID,
			Message: firstNonEmptyString(payload.Msg, strings.TrimSpace(string(respBody))),
			ResetAt: fmt.Sprintf("code=%d", payload.Code),
		}, nil
	}
}
