package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Raccoon's one-time "first login" rewards (首登奖励).
//
// Upstream has no task table and — this is the part that shapes everything
// below — no read endpoint that answers "does this account still have its
// first-login reward". What it has is two grant endpoints, each firing once per
// account on a different identity branch:
//
//	POST {main}/api/web/desktop/v1/login/points/grant   desktop identity headers
//	POST {main}/api/web/mobile/v1/login/points/grant    app (Capacitor) headers
//
//	→ {code:0, data:{granted:true, popup:{source, points, expire_at}}}
//	  {code:0, data:{granted:false}}                     already paid out
//
// The nominal value is 3000 points each, and the real number is whatever
// popup.points says. The two branches do not share an allowance: an account
// whose desktop reward the official client already collected can still take the
// phone one, which is exactly the case for an account pool fed from desktop
// logins.
//
// ── Why there is no ledger here ─────────────────────────────────────────
// Both endpoints are idempotent and cheap, and `granted` tells the truth on
// every call, so this client keeps no persistent state for them: a repeated
// claim answers AlreadyClaimed instead of silently adding points twice. What it
// does keep is a per-process memo (resolvedGrants) so the daily check-in hook
// does not re-post two doomed calls per account per day for the rest of the
// deployment's life. The memo dying with the process is fine — it only costs one
// extra pair of calls after a restart.

const (
	// desktopGrantPath and mobileGrantPath are the two one-time grant entries.
	desktopGrantPath = "/api/web/desktop/v1/login/points/grant"
	mobileGrantPath  = "/api/web/mobile/v1/login/points/grant"

	// TaskDesktopLoginGrant / TaskMobileLoginGrant double as the plan_id the
	// console hands back to Claim, so they must stay stable.
	TaskDesktopLoginGrant = "desktop_login_grant"
	TaskMobileLoginGrant  = "mobile_login_grant"

	// nominalGrantPoints is the advertised value, used only when upstream
	// omits popup.points (the reference implementation does the same).
	nominalGrantPoints = 3000

	// grantTimeout bounds one grant call; the main site is slow behind the
	// proxy used from outside mainland China.
	grantTimeout = 20 * time.Second
)

// loginGrant describes one claimable first-login reward.
type loginGrant struct {
	task    string
	label   string
	path    string
	headers map[string]string
}

// loginGrants returns both rewards in a fixed order: desktop first (the one an
// existing account is most likely to have already taken), phone second.
func loginGrants() []loginGrant {
	return []loginGrant{
		{
			task:    TaskDesktopLoginGrant,
			label:   "电脑端首登奖励",
			path:    desktopGrantPath,
			headers: desktopCheckinHeaders(),
		},
		{
			task:    TaskMobileLoginGrant,
			label:   "手机端首登奖励",
			path:    mobileGrantPath,
			headers: mobileClientHeaders(),
		},
	}
}

// mobileClientHeaders mimic the Capacitor app's identity: the platform is the
// app shell's getPlatform() with an `app-` prefix and the version keeps its `v`.
// Upstream settles the phone reward against this branch, so both must be
// present; reporting android v1.0.3 is the measured sample.
func mobileClientHeaders() map[string]string {
	return map[string]string{
		"X-Client-Platform":  "app-android",
		"X-Client-Version":   "v1.0.3",
		"X-Raccoon-Language": "zh",
	}
}

// PreviewClaim lists the two first-login rewards.
//
// Status is "unknown" on purpose: upstream cannot say whether a given branch is
// still unclaimed, so pretending otherwise would show a green claim button on an
// account that has nothing left. Claim resolves it for real in one call.
func (c *Client) PreviewClaim(ctx context.Context, accountID string) ([]providers.ClaimPlan, error) {
	if _, err := c.credential(ctx, accountID); err != nil {
		return nil, err
	}
	plans := make([]providers.ClaimPlan, 0, 2)
	for _, grant := range loginGrants() {
		status := "unknown"
		if c.grantResolved(accountID, grant.task) {
			status = "claimed"
		}
		plans = append(plans, providers.ClaimPlan{
			PlanID: grant.task,
			Name:   grant.label,
			Status: status,
		})
	}
	return plans, nil
}

// Claim claims one first-login reward by its task id.
func (c *Client) Claim(ctx context.Context, accountID, planID string) (providers.ClaimResult, error) {
	wanted := strings.TrimSpace(planID)
	for _, grant := range loginGrants() {
		if grant.task == wanted {
			outcome, err := c.claimLoginGrant(ctx, accountID, grant)
			if err != nil {
				return providers.ClaimResult{}, err
			}
			return outcome.result, nil
		}
	}
	return providers.ClaimResult{}, fmt.Errorf(
		"raccoon has no claimable task %q (expected %s or %s)",
		planID, TaskDesktopLoginGrant, TaskMobileLoginGrant)
}

// grantOutcome is one branch's answer: the ClaimResult the console shows plus
// the points upstream actually granted (0 when nothing landed).
type grantOutcome struct {
	result providers.ClaimResult
	points int64
}

// claimLoginGrant fires one grant endpoint and reports what it said.
func (c *Client) claimLoginGrant(ctx context.Context, accountID string, grant loginGrant) (grantOutcome, error) {
	if c.grantResolved(accountID, grant.task) {
		return grantOutcome{result: providers.ClaimResult{
			AlreadyClaimed: true,
			PlanID:         grant.task,
			Message:        grant.label + "：本次进程内已确认领过",
		}}, nil
	}
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return grantOutcome{}, err
	}
	token, err := c.siteToken(ctx, accountID, credential)
	if err != nil {
		return grantOutcome{}, err
	}
	data, err := c.postJSON(ctx, accountID, grant.path, token, grant.headers, grantTimeout)
	if errors.Is(err, errSessionExpired) {
		// A stored access token can simply be stale; refresh once, exactly like
		// the check-in trigger does.
		if refreshed, refreshErr := c.refresh(ctx, accountID, credential); refreshErr == nil {
			if next := strings.TrimSpace(refreshed.AccessToken); next != "" {
				data, err = c.postJSON(ctx, accountID, grant.path, next, grant.headers, grantTimeout)
			}
		}
	}
	if err != nil {
		return grantOutcome{}, fmt.Errorf("小浣熊%s：%w", grant.label, err)
	}

	var payload struct {
		Granted bool `json:"granted"`
		Popup   *struct {
			Source   string `json:"source"`
			Points   int64  `json:"points"`
			ExpireAt string `json:"expire_at"`
		} `json:"popup"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return grantOutcome{}, fmt.Errorf("小浣熊%s：响应无法解析", grant.label)
	}
	if !payload.Granted {
		// Both "already paid out" and "this account never qualified" land here,
		// and both mean the same thing for the caller: nothing more to get.
		c.markGrantResolved(accountID, grant.task)
		return grantOutcome{result: providers.ClaimResult{
			AlreadyClaimed: true,
			PlanID:         grant.task,
			Message:        grant.label + "：上游 granted=false（已领过或不符合条件）",
		}}, nil
	}
	points := int64(nominalGrantPoints)
	if payload.Popup != nil && payload.Popup.Points > 0 {
		points = payload.Popup.Points
	}
	c.markGrantResolved(accountID, grant.task)
	return grantOutcome{
		result: providers.ClaimResult{
			Claimed: true,
			PlanID:  grant.task,
			Message: fmt.Sprintf("%s：+%d 分", grant.label, points),
		},
		points: points,
	}, nil
}

// ClaimLoginGrants fires both one-time rewards and reports what landed.
//
// Best-effort by design: it runs right after a successful daily check-in, and a
// reward endpoint that is having a bad day must never turn a good check-in into
// a failure. Callers get the points that did arrive plus one note per branch
// that refused, and decide what to do with them.
func (c *Client) ClaimLoginGrants(ctx context.Context, accountID string) (points int64, notes []string) {
	for _, grant := range loginGrants() {
		outcome, err := c.claimLoginGrant(ctx, accountID, grant)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s失败：%v", grant.label, err))
			continue
		}
		if !outcome.result.Claimed {
			continue
		}
		points += outcome.points
		notes = append(notes, outcome.result.Message)
	}
	return points, notes
}

// grantResolved reports whether this process already settled a task for the
// account, so the check-in hook stops re-posting it every morning.
func (c *Client) grantResolved(accountID, task string) bool {
	c.grantsMu.Lock()
	defer c.grantsMu.Unlock()
	return c.resolvedGrants[grantKey(accountID, task)]
}

func (c *Client) markGrantResolved(accountID, task string) {
	c.grantsMu.Lock()
	defer c.grantsMu.Unlock()
	if c.resolvedGrants == nil {
		c.resolvedGrants = make(map[string]bool)
	}
	c.resolvedGrants[grantKey(accountID, task)] = true
}

func grantKey(accountID, task string) string { return accountID + "\x00" + task }

// postJSON performs a POST with an optional JSON body and unwraps the
// {code, message, data} envelope the same way requestJSON does for GETs.
func (c *Client) postJSON(ctx context.Context, accountID, path, token string, headers map[string]string, timeout time.Duration) (json.RawMessage, error) {
	endpoint := c.mainOrigin() + path
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Upstream's grant entries read no body, but a POST with no body at all is
	// rejected by the edge, so send an empty object.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	// Honour the account proxy for the same reason the check-in chain does: the
	// main site drops the TCP connect when this gateway runs outside mainland
	// China, while the LLM gateway stays reachable.
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("请求超时")
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errSessionExpired
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("响应无法解析")
	}
	if envelope.Code != 0 {
		message := firstNonEmpty(envelope.Message, envelope.Msg)
		if message == "" {
			message = fmt.Sprintf("上游返回异常 code=%d", envelope.Code)
		}
		return nil, fmt.Errorf("%s", message)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return raw, nil
	}
	return envelope.Data, nil
}
