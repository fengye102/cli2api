package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
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

// The promo's plan id used to be predictable (`zcode-v3-start-plan-<MMDD>-wk`),
// which let the sweep guess it and beat the crowd. It is not predictable any
// more: ZCode hands out gift plans through the app's own claim banners, whose
// plan ids live in the marketing feed and in neither billing list while the
// banner is up (a "ZCODE WEEKEND BUILD" banner on 10-30 was in no billing view
// at all). The feed ships each banner's claim button as
//
//	{"type":"claim_zcode_plan","args":{"plan_id":"..."}}
//
// with the display name in the banner's `zcode_plan.name`, so the sweep asks the
// feed first and keeps the date-encoded guesses as a fallback for the rounds
// where the feed is down or already drained.
const (
	// marketingTouchPath is the app's banner feed, outside the plan gateway's
	// /zcode-plan prefix.
	marketingTouchPath = "/marketing/touch"
	// claimBannerAction is the banner button action that carries a plan id.
	claimBannerAction = "claim_zcode_plan"
	// bannerWalkDepth bounds the search for a claim action inside one delivery:
	// the capture nests it about five levels down, and a bound keeps a
	// pathological feed from turning into a stack.
	bannerWalkDepth = 12
)

// marketingTouchSeq counts feed reads the way the app does (from 0 per
// process). The upstream uses it to tell a fresh poll from a replay.
var marketingTouchSeq atomic.Int64

// clientLanguage is the X-Client-Language the app sends the feed: the system
// locale, POSIX form and without the encoding suffix.
func clientLanguage() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" || value == "C" || value == "POSIX" {
			continue
		}
		if idx := strings.IndexAny(value, ".@"); idx >= 0 {
			value = value[:idx]
		}
		return strings.ReplaceAll(value, "_", "-")
	}
	return "en-US"
}

// marketingTouchURL builds the feed URL. It hangs off /api/v1, not the plan
// gateway's /api/v1/zcode-plan, so the test override is applied to the host.
func (c *Client) marketingTouchURL(seq int) string {
	base := strings.TrimSuffix(gatewayBaseURL, "/zcode-plan")
	if override := strings.TrimSpace(c.catalogURL); override != "" {
		base = strings.TrimRight(override, "/") + strings.TrimSuffix(gatewayPathPrefix, "/zcode-plan")
	}
	return fmt.Sprintf("%s%s?seq=%d", base, marketingTouchPath, seq)
}

// bannerPlanIDs reads the account's banner feed and returns the plan ids its
// claim buttons carry, in feed order, each once.
func (c *Client) bannerPlanIDs(ctx context.Context, accountID string, cred Credential) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.marketingTouchURL(int(marketingTouchSeq.Add(1)-1)), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range defaultHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Client-Language", clientLanguage())
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
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("zcode marketing feed status=%d", status)
	}
	return parseBannerPlanIDs(body)
}

// parseBannerPlanIDs walks the feed for claim buttons. An unreadable feed is an
// error, never an empty list: "the feed lists nothing" and "the feed did not
// answer" must not read the same.
func parseBannerPlanIDs(body []byte) ([]string, error) {
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Deliveries []any `json:"deliveries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("zcode marketing feed is not json: %w", err)
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("zcode marketing feed code=%d", payload.Code)
	}
	out := make([]string, 0, len(payload.Data.Deliveries))
	seen := map[string]bool{}
	for _, delivery := range payload.Data.Deliveries {
		walkClaimBanners(delivery, 0, func(planID string) {
			if !seen[planID] {
				seen[planID] = true
				out = append(out, planID)
			}
		})
	}
	return out, nil
}

// walkClaimBanners calls visit for every claim action inside v, wherever the
// banner keeps it.
func walkClaimBanners(v any, depth int, visit func(string)) {
	if depth > bannerWalkDepth {
		return
	}
	switch node := v.(type) {
	case []any:
		for _, item := range node {
			walkClaimBanners(item, depth+1, visit)
		}
	case map[string]any:
		if node["type"] == claimBannerAction {
			if args, ok := node["args"].(map[string]any); ok {
				if planID, ok := args["plan_id"].(string); ok {
					if trimmed := strings.TrimSpace(planID); trimmed != "" {
						visit(trimmed)
					}
				}
			}
		}
		for _, child := range node {
			walkClaimBanners(child, depth+1, visit)
		}
	}
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

	// The feed's own ids come first: they are what the app would claim, and a
	// banner plan often has an id the date scheme cannot predict. A feed that
	// does not read is noted and the date guesses carry the round.
	feedNote := ""
	candidates := weekendPlanCandidates(time.Now())
	var feedIDs []string
	if cred, err := c.resolvedCredential(ctx, accountID); err == nil {
		if ids, feedErr := c.bannerPlanIDs(ctx, accountID, cred); feedErr != nil {
			feedNote = feedErr.Error()
		} else {
			feedIDs = ids
		}
	}
	if len(feedIDs) > 0 {
		candidates = append(feedIDs, candidates...)
		candidates = dedupePlanIDs(candidates)
	}

	var last autoClaimResult
	for _, planID := range candidates {
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
			// Unknown or unlisted code: surface it rather than loop, and never
			// log an empty message.
			if strings.TrimSpace(result.Message) == "" {
				result.Message = fmt.Sprintf("plan %s refused (code=%d)", planID, last.Code)
			}
			return result, nil
		}
	}
	if last.PlanID != "" {
		message := fmt.Sprintf("no weekend plan claimed this round (last code %d on %s)", last.Code, last.PlanID)
		if feedNote != "" {
			message += "; banner feed unread: " + feedNote
		}
		return providers.ClaimResult{
			PlanID:  last.PlanID,
			Message: message,
			ResetAt: fmt.Sprintf("code=%d", last.Code),
		}, nil
	}
	message := "no weekend plan claimed this round"
	if feedNote != "" {
		message += "; banner feed unread: " + feedNote
	}
	return providers.ClaimResult{Message: message, ResetAt: "code=1005"}, nil
}

// dedupePlanIDs keeps the first appearance of each id, feed order first.
func dedupePlanIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// WeekendClaimWindowOpen lets the maintenance sweep stay quiet outside the
// promo window instead of posting a claim every ten minutes all week: upstream
// answers those with 1005 and, once they pile up, a 429 with no body at all.
func (c *Client) WeekendClaimWindowOpen(now time.Time) bool {
	return WeekendClaimOpen(now)
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

	// An empty 2xx body unmarshals to code 0, which is also the success code: a
	// gateway hiccup would then read as "claimed" and the sweep would stop for
	// the whole weekend believing the account holds a plan it never got. A real
	// success names the plan (or at least says something), so require evidence.
	claimedWithEvidence := payload.Code == weekendClaimCodeClaimed &&
		(payload.Data.Plan.PlanID != "" || strings.TrimSpace(payload.Msg) != "")

	switch {
	case status >= 200 && status < 300 && claimedWithEvidence:
		return providers.ClaimResult{Claimed: true, PlanID: planID, Message: firstNonEmptyString(payload.Msg, "claimed")}, nil
	case payload.Code == weekendClaimCodeDup || strings.Contains(strings.ToLower(payload.Msg), "already claimed"):
		return providers.ClaimResult{AlreadyClaimed: true, PlanID: planID, Message: firstNonEmptyString(payload.Msg, "already claimed")}, nil
	case status == http.StatusTooManyRequests:
		// Upstream pushes back when claims arrive faster than it likes (it
		// answers with no body at all). Name it, stop the round, and let the
		// next sweep — ten minutes on — come back.
		return providers.ClaimResult{
			PlanID:  planID,
			Message: "claim rate limited by upstream (status=429); waiting for the next round",
			ResetAt: "code=429",
		}, nil
	case payload.Code == 3007 || strings.Contains(strings.ToLower(payload.Msg), "captcha"):
		// Same meaning as Claim's 3007: upstream wants an interactive check we
		// cannot mint, so tag it and let the sweep stop round-tripping.
		return providers.ClaimResult{}, fmt.Errorf("zcode claim captcha failed: %s: %w",
			firstNonEmptyString(payload.Msg, "captcha required"), providers.ErrNeedsHumanVerification)
	default:
		// Business failure with the raw code embedded for the sweep. An empty
		// body is a real answer here (upstream sometimes refuses a claim with
		// nothing at all), so the message names the status instead of leaving
		// the sweep's log line ending in a bare colon.
		message := firstNonEmptyString(payload.Msg, strings.TrimSpace(string(respBody)))
		if message == "" {
			message = fmt.Sprintf("empty upstream body (status=%d)", status)
		}
		return providers.ClaimResult{
			PlanID:  planID,
			Message: message,
			ResetAt: fmt.Sprintf("code=%d", payload.Code),
		}, nil
	}
}
