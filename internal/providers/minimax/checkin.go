package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Daily sign-in is a seven-day cycle the agent client renders as a panel: 400
// points a day, 1000 on days four and seven. The endpoints below are the ones
// the bundled CLI's /checkin command hits.
//
// The gateway insists on a timezone_id QUERY parameter. Sent as a header, or
// omitted, a perfectly valid token still answers
// {"status_code":1406010011,"status_msg":"invalid timezone_id"}.
const (
	signinDayUnavailable = 1
	signinDayClaimable   = 2
	signinDayClaimed     = 3

	claimResultClaimed        = 1
	claimResultAlreadyClaimed = 2

	// defaultCheckinTimezone is the day boundary the console assumes; an
	// operator in another zone overrides it with the env var below.
	defaultCheckinTimezone = "Asia/Shanghai"
	checkinTimezoneEnv     = "CLI2API_MINIMAX_CHECKIN_TZ"
)

// signinDay is one day of the cycle. status: 1 = not reached, 2 = claimable,
// 3 = claimed.
type signinDay struct {
	DayNo       flexNumber `json:"day_no"`
	Points      flexNumber `json:"points"`
	BonusPoints flexNumber `json:"bonus_points"`
	Status      flexNumber `json:"status"`
	IsToday     bool       `json:"is_today"`
}

type signinPanel struct {
	Data struct {
		Scene flexNumber  `json:"scene"`
		Days  []signinDay `json:"days"`
	} `json:"data"`
	BaseResp signinBaseResp `json:"base_resp"`
}

type signinBaseResp struct {
	StatusCode flexNumber `json:"status_code"`
	StatusMsg  string     `json:"status_msg"`
}

type signinClaim struct {
	Data struct {
		ClaimResult flexNumber `json:"claim_result"`
		DayNo       flexNumber `json:"day_no"`
		Points      flexNumber `json:"points"`
		ExpireAtMS  flexNumber `json:"expire_at_ms"`
		ClaimID     string     `json:"claim_id"`
	} `json:"data"`
	BaseResp signinBaseResp `json:"base_resp"`
}

// today returns the entry the panel marks as today's.
func (panel signinPanel) today() (signinDay, bool) {
	for _, day := range panel.Data.Days {
		if day.IsToday {
			return day, true
		}
	}
	return signinDay{}, false
}

// checkinTimezone resolves the day boundary for an account. MiniMax bills the
// claim against the caller's calendar day, so the value decides which day of
// the cycle is "today".
func checkinTimezone(_ string) string {
	override := strings.TrimSpace(os.Getenv(checkinTimezoneEnv))
	if override == "" {
		return defaultCheckinTimezone
	}
	if _, err := time.LoadLocation(override); err != nil {
		return defaultCheckinTimezone
	}
	return override
}

// Checkin claims today's sign-in reward. already/skipped are normal outcomes:
// the scheduler calls this once per day and must not treat "done already" as a
// failure.
func (c *Client) Checkin(ctx context.Context, accountID string) (providers.CheckinResult, error) {
	cred, err := c.freshCredential(ctx, accountID)
	if err != nil {
		return providers.CheckinResult{}, err
	}
	zone := checkinTimezone(cred.Region)
	checkinCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	// Read the balance first so the message reports what actually landed
	// rather than the nominal face value of the reward.
	before, beforeOK := c.remainingCredits(checkinCtx, accountID, cred)

	panel, err := c.signinStatus(checkinCtx, accountID, cred, zone)
	if err != nil {
		return providers.CheckinResult{}, err
	}
	today, ok := panel.today()
	if !ok {
		return providers.CheckinResult{
			Status:  "skipped",
			Message: fmt.Sprintf("MiniMax 签到面板没有今天（scene=%v）", panel.Data.Scene),
		}, nil
	}
	switch today.Status.float() {
	case signinDayClaimed:
		return providers.CheckinResult{
			Status:  "already",
			Message: fmt.Sprintf("MiniMax 今天已签到（第 %v 天，+%v 积分）", today.DayNo, today.Points),
		}, nil
	case signinDayClaimable:
		// fall through to the claim
	default:
		return providers.CheckinResult{
			Status:  "skipped",
			Message: fmt.Sprintf("MiniMax 第 %v 天还不可签（status=%v）", today.DayNo, today.Status),
		}, nil
	}

	claim, err := c.signinClaim(checkinCtx, accountID, cred, zone)
	if err != nil {
		return providers.CheckinResult{}, err
	}
	if claim.Data.ClaimResult.float() == claimResultAlreadyClaimed {
		return providers.CheckinResult{
			Status:  "already",
			Message: fmt.Sprintf("MiniMax 今天已签到（第 %v 天）", claim.Data.DayNo),
		}, nil
	}
	if claim.Data.ClaimResult.float() != claimResultClaimed {
		return providers.CheckinResult{}, fmt.Errorf("minimax 签到失败：claim_result=%v", claim.Data.ClaimResult)
	}

	reward := claim.Data.Points.float()
	message := fmt.Sprintf("MiniMax 签到成功：第 %v 天 +%v", claim.Data.DayNo, reward)
	reported := reward
	if after, afterOK := c.remainingCredits(checkinCtx, accountID, cred); beforeOK && afterOK {
		message += fmt.Sprintf("，余额 %s → %s", trimFloat(before), trimFloat(after))
		reported = after - before
	}
	if expires := int64(claim.Data.ExpireAtMS.float() / 1000); expires > 0 {
		// The expiry is what the operator plans around, so print it in the
		// same zone the claim was filed under rather than the host's.
		loc, err := time.LoadLocation(zone)
		if err != nil {
			loc = time.UTC
		}
		message += fmt.Sprintf("，本次积分 %s 到期", time.Unix(expires, 0).In(loc).Format("2006-01-02"))
	}
	return providers.CheckinResult{Status: "success", Message: message, RewardCredits: reported}, nil
}

// signinStatus reads the seven-day panel. A non-zero base_resp code is a real
// failure: the CLI would render nothing, and silently reporting "already
// checked in" would hide a broken account.
func (c *Client) signinStatus(ctx context.Context, accountID string, cred Credential, zone string) (signinPanel, error) {
	body, err := c.signinRequest(ctx, accountID, cred, http.MethodGet, SigninStatusPath, zone, nil)
	if err != nil {
		return signinPanel{}, err
	}
	var panel signinPanel
	if err := json.Unmarshal(body, &panel); err != nil {
		return signinPanel{}, fmt.Errorf("minimax 签到面板无法解析：%w", err)
	}
	if code := panel.BaseResp.StatusCode.float(); code != 0 {
		return signinPanel{}, fmt.Errorf("minimax 签到面板失败：code=%v %s", code, strings.TrimSpace(panel.BaseResp.StatusMsg))
	}
	return panel, nil
}

// signinClaim claims today's reward. The CLI posts an empty object, and the
// gateway adopts the same day the status call just reported.
func (c *Client) signinClaim(ctx context.Context, accountID string, cred Credential, zone string) (signinClaim, error) {
	body, err := c.signinRequest(ctx, accountID, cred, http.MethodPost, SigninClaimPath, zone, []byte("{}"))
	if err != nil {
		return signinClaim{}, err
	}
	var claim signinClaim
	if err := json.Unmarshal(body, &claim); err != nil {
		return signinClaim{}, fmt.Errorf("minimax 签到响应无法解析：%w", err)
	}
	if code := claim.BaseResp.StatusCode.float(); code != 0 {
		return signinClaim{}, fmt.Errorf("minimax 签到失败：code=%v %s", code, strings.TrimSpace(claim.BaseResp.StatusMsg))
	}
	return claim, nil
}

func (c *Client) signinRequest(ctx context.Context, accountID string, cred Credential, method, path, zone string, body []byte) ([]byte, error) {
	site := c.site(cred.Region)
	endpoint := site.AgentHost + path + "?timezone_id=" + url.QueryEscape(zone)
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	applyAuthHeaders(req.Header, cred)
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimax 签到不可达：%w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("minimax 签到读取失败：%w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("minimax 签到鉴权失败 HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("minimax 签到 HTTP %d：%s", resp.StatusCode, clip(string(payload), 200))
	}
	return payload, nil
}

// remainingCredits is the best-effort balance probe used to report the real
// delta. A failure is not fatal: the claim already happened.
func (c *Client) remainingCredits(ctx context.Context, accountID string, cred Credential) (float64, bool) {
	body, err := c.fetchCreditDetails(ctx, accountID, cred)
	if err != nil {
		return 0, false
	}
	info, err := parseCreditDetails(body, c.clock())
	if err != nil {
		return 0, false
	}
	return info.Remaining, true
}

// trimFloat prints 800 rather than 800.000000 in the console message.
func trimFloat(value float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", value), "0"), ".")
}
