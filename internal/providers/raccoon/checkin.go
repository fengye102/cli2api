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

// Daily check-in for Raccoon (小浣熊).
//
// Raccoon has no "check in" button of its own. Its daily credit is granted by
// the **desktop client login** chain: the official desktop app hits
// setting_info on every launch, and the server settles the day's points from
// that call (idempotent per calendar day). Replaying that same call is what
// "checking in" means here:
//
//	GET  {main}/api/web/office/v3/setting_info      ← the daily grant trigger
//	GET  {main}/api/web/points/v1/bills?paging…     ← reconcile today's credit
//
// Both need the desktop identity headers, and the grant notification comes back
// in the setting_info body (point_grant_popups / point_grant_toast).
//
// ── Why the bills call decides success ──────────────────────────────────
// setting_info answers 200 whether or not anything was granted: the grant is
// idempotent, so a second call in the same day looks exactly like a first one
// that granted. Success is therefore "today's credit is actually visible in the
// bill", not "this HTTP call returned 200" — a check-in that reports success
// without evidence would let a silently-broken account look healthy forever.
// The bills endpoint is slow (≈9s for limit=20), so it gets its own timeout.
//
// ── Why these calls are direct, not proxied ─────────────────────────────
// The reference implementation reaches this chain with a bare fetch: the
// account-level proxy exists for the long streaming chat requests, and the main
// site is a different host from the LLM gateway. Attaching the account proxy
// here would diverge from the verified upstream behaviour, so these requests go
// out directly (c.http) like the balance queries do (see quota.go).

const (
	// settingInfoPath is the desktop login entry that settles the daily grant.
	settingInfoPath = "/api/web/office/v3/setting_info"
	// billsPath is the credit ledger used to confirm today's grant landed.
	billsPath = "/api/web/points/v1/bills"

	// billsLimit is how many recent ledger rows to scan (reference value).
	billsLimit = 20

	// settingInfoTimeout bounds the grant trigger.
	settingInfoTimeout = 20 * time.Second
	// billsTimeout is longer because the ledger endpoint is slow upstream.
	billsTimeout = 25 * time.Second

	// maxBodyBytes caps a hostile or oversized response.
	maxBodyBytes = 1 << 20
)

// beijingZone is the calendar the upstream settles on. The daily grant rolls
// over at UTC+8 midnight, so "today" must be read in that zone regardless of
// where the container runs: on a UTC host the 8-hour offset would file a
// morning grant under yesterday and make "today's credit" permanently empty.
var beijingZone = time.FixedZone("CST", 8*60*60)

// errSessionExpired marks a 401 from upstream. The check-in path catches it to
// force one token refresh and retry, the same way chat does.
var errSessionExpired = errors.New("登录态已过期，请重新登录")

// Checkin replays the desktop login grant and reports whether today's credit
// actually landed.
func (c *Client) Checkin(ctx context.Context, accountID string) (providers.CheckinResult, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.CheckinResult{}, err
	}
	token, err := c.siteToken(ctx, accountID, credential)
	if err != nil {
		return providers.CheckinResult{}, err
	}

	// ① Grant trigger (idempotent per day). A failure here is a real failure:
	//    the day's grant did not happen, so it must not be reported as success.
	grant, err := c.requestJSON(ctx, accountID, http.MethodGet, settingInfoPath, token, desktopCheckinHeaders(), settingInfoTimeout)
	if err != nil {
		// A stored access token can simply be stale. Refresh once and retry,
		// exactly like the chat path: without this, every account whose token
		// aged out would report a permanent failure until someone re-logged in
		// by hand.
		if errors.Is(err, errSessionExpired) {
			refreshed, refreshErr := c.refresh(ctx, accountID, credential)
			if refreshErr == nil && strings.TrimSpace(refreshed.AccessToken) != "" {
				token = strings.TrimSpace(refreshed.AccessToken)
				grant, err = c.requestJSON(ctx, accountID, http.MethodGet, settingInfoPath, token, desktopCheckinHeaders(), settingInfoTimeout)
			}
		}
		if err != nil {
			return providers.CheckinResult{}, fmt.Errorf("小浣熊签到：发放触发失败：%w", err)
		}
	}

	// ② Ledger reconciliation. This is the authority on whether points landed.
	grants, err := c.todayGrants(ctx, accountID, token)
	if err != nil {
		// Without the ledger we cannot honestly claim success.
		return providers.CheckinResult{}, fmt.Errorf("小浣熊签到：无法核对今日入账：%w", err)
	}

	granted := int64(0)
	for _, item := range grants {
		granted += item.Points
	}
	if granted > 0 {
		message := fmt.Sprintf("今日积分 +%d", granted)
		if note := grantNote(grant); note != "" {
			message += "；" + note
		}
		return providers.CheckinResult{
			Status:        "success",
			Message:       message,
			RewardCredits: float64(granted),
		}, nil
	}
	// The trigger returned 200 but no credit is visible for today. Saying
	// "already" here would tell the operator there is nothing to do when the
	// grant in fact did not settle, so this is surfaced as an error and the
	// scheduled retry gets another chance.
	return providers.CheckinResult{}, fmt.Errorf("小浣熊签到：今日未见积分入账（发放触发未生效，稍后重试）")
}

// siteToken returns an access token for the main-site calls (check-in, quota),
// minting one from the refresh token when the stored credential only holds that.
func (c *Client) siteToken(ctx context.Context, accountID string, credential Credential) (string, error) {
	if token := strings.TrimSpace(credential.AccessToken); token != "" {
		return token, nil
	}
	if strings.TrimSpace(credential.RefreshToken) == "" {
		return "", fmt.Errorf("raccoon site call needs a token or refresh token")
	}
	refreshed, err := c.refresh(ctx, accountID, credential)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(refreshed.AccessToken)
	if token == "" {
		return "", fmt.Errorf("raccoon site call: refresh returned no access token")
	}
	return token, nil
}

// desktopCheckinHeaders are the identity headers the official desktop client
// sends. The upstream settles the daily grant against the desktop branch, so
// the platform and version must be present and must say desktop.
func desktopCheckinHeaders() map[string]string {
	return map[string]string{
		"X-Client-Platform":  "desktop-windows",
		"X-Client-Version":   "v1.0.0",
		"X-Raccoon-Language": "zh",
	}
}

// requestJSON performs a GET and unwraps the {code, message, data} envelope.
func (c *Client) requestJSON(ctx context.Context, accountID, method, path, token string, headers map[string]string, timeout time.Duration) (json.RawMessage, error) {
	endpoint := c.mainOrigin() + path
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	// Direct connection: see the module comment on why the account proxy is
	// deliberately not attached here.
	resp, err := c.http.Do(req)
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

// billEntry is one row of the credit ledger.
type billEntry struct {
	Name   string
	Points int64
	At     string
}

// todayGrants returns the credit rows whose date part is today in Beijing time.
func (c *Client) todayGrants(ctx context.Context, accountID, token string) ([]billEntry, error) {
	path := fmt.Sprintf("%s?paging.limit=%d&paging.offset=0", billsPath, billsLimit)
	data, err := c.requestJSON(ctx, accountID, http.MethodGet, path, token, nil, billsTimeout)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Items []struct {
			Points    json.Number `json:"points"`
			CreatedAt string      `json:"created_at"`
			EventName string      `json:"event_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("账单响应无法解析")
	}
	today := time.Now().In(beijingZone).Format("2006-01-02")
	entries := make([]billEntry, 0, len(payload.Items))
	for _, item := range payload.Items {
		points, err := item.Points.Int64()
		if err != nil || points <= 0 {
			continue
		}
		// The ledger timestamps are Beijing-time strings; only the date part
		// matters, so it is compared as text rather than parsed (a parse would
		// have to guess the layout and could silently shift the day).
		if len(item.CreatedAt) < 10 || item.CreatedAt[:10] != today {
			continue
		}
		name := item.EventName
		if strings.TrimSpace(name) == "" {
			name = "积分发放"
		}
		at := ""
		if len(item.CreatedAt) >= 19 {
			at = item.CreatedAt[11:19]
		}
		entries = append(entries, billEntry{Name: name, Points: points, At: at})
	}
	return entries, nil
}

// grantNote renders the upstream grant notification, when it sent one.
func grantNote(grant json.RawMessage) string {
	if len(grant) == 0 {
		return ""
	}
	var payload struct {
		Popups json.RawMessage `json:"point_grant_popups"`
		Toast  json.RawMessage `json:"point_grant_toast"`
	}
	if err := json.Unmarshal(grant, &payload); err != nil {
		return ""
	}
	if text := strings.TrimSpace(string(payload.Toast)); text != "" && text != "null" {
		var toast string
		if err := json.Unmarshal(payload.Toast, &toast); err == nil && strings.TrimSpace(toast) != "" {
			return strings.TrimSpace(toast)
		}
	}
	if text := strings.TrimSpace(string(payload.Popups)); text != "" && text != "null" && text != "[]" {
		return "上游发放通知已下发"
	}
	return ""
}
