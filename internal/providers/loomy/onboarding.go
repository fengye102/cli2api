package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Onboarding tasks: 10,000 credits for eight bare API calls.
//
// Endpoints on the model gateway:
//
//	GET  /onboarding/tasks          → {tasks:{key:bool}, earned, total}
//	POST /onboarding/tasks/complete  body {key} → {alreadyCompleted, balance}
//
// Completion is judged locally by the official Electron client; the server only
// records an idempotent flag and credits the points. So every task here is
// claimable without sending a single token — measured on a fresh account, which
// went from 0 to 10,000 credits.
//
// Three semantics carried over from the reference implementation, each of which
// was measured rather than chosen for style:
//
//   - idempotency is read from the response's alreadyCompleted, not from the
//     HTTP status (a replay is a success);
//   - the server's `earned` is recomputed locally from the task table, because
//     it has historically disagreed with the client's own table;
//   - the request body carries only {key} — no device fingerprint, no version.

// onboardingTask is one row of the task table.
type onboardingTask struct {
	Key    string
	Points int
	Title  string
}

// onboardingTasks is the table the official client uses; the sum is exactly
// 10,000.
var onboardingTasks = []onboardingTask{
	{"first_message", 500, "发送你的第一条消息"},
	{"pick_skill", 1000, "试试选择一个技能"},
	{"generate_ppt", 1500, "生成第一份 PPT"},
	{"set_schedule", 1000, "设置定时任务"},
	{"install_skill", 1500, "在技能广场安装一个技能"},
	{"configure_remote", 1000, "配置远程控制"},
	{"create_soul", 1500, "创建你的第一个搭子"},
	{"share_soul", 2000, "把搭子分享给朋友"},
}

const (
	// claimAllPlanID is the synthetic plan that claims every outstanding task in
	// one console click. It is not an upstream id.
	claimAllPlanID = "onboarding_all"
	// taskPlanPrefix namespaces a single task's plan id.
	taskPlanPrefix = "task:"
)

func onboardingTaskByKey(key string) (onboardingTask, bool) {
	for _, task := range onboardingTasks {
		if task.Key == key {
			return task, true
		}
	}
	return onboardingTask{}, false
}

// onboardingCall performs one onboarding request and unwraps {code, desc, data}.
// A 401 or the 100002 business code is reported as an auth error so the caller
// can relogin instead of retrying eight doomed calls.
func (c *Client) onboardingCall(ctx context.Context, accountID, session, path, method string, body map[string]any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(payload)
	}
	ctx, cancel := context.WithTimeout(ctx, onboardingTimeout)
	defer cancel()

	endpoint := strings.TrimRight(c.modelBase, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	for key, value := range modelHeaders(session) {
		req.Header.Set(key, value)
	}
	client, err := c.modelClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &providers.Error{Kind: "unavailable", Message: "loomy 新手任务接口不可达：" + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &providers.Error{Kind: "auth", Status: resp.StatusCode, Message: "loomy session 已失效"}
	}
	if resp.StatusCode >= 300 {
		return nil, newProviderError(resp.StatusCode, string(raw))
	}

	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Desc    string          `json:"desc"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &providers.Error{Kind: "unavailable", Message: "loomy 新手任务响应无法解析"}
	}
	code := strings.Trim(strings.TrimSpace(string(envelope.Code)), `"`)
	if code != "" && code != onboardingOKCode {
		message := firstNonEmpty(envelope.Desc, envelope.Message, code)
		return nil, &providers.Error{
			Kind:    "unavailable",
			Status:  resp.StatusCode,
			Message: path + "：" + message,
			Code:    code,
		}
	}
	var data map[string]any
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		_ = json.Unmarshal(envelope.Data, &data)
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// onboardingState reads which tasks are already done.
func (c *Client) onboardingState(ctx context.Context, accountID, session string) (map[string]bool, error) {
	data, err := c.onboardingCall(ctx, accountID, session, onboardingTasksPath, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	done := make(map[string]bool, len(onboardingTasks))
	raw, _ := data["tasks"].(map[string]any)
	for _, task := range onboardingTasks {
		value, _ := raw[task.Key].(bool)
		done[task.Key] = value
	}
	return done, nil
}

// claimTask posts one completion and reports whether the server says it was
// already claimed.
func (c *Client) claimTask(ctx context.Context, accountID, session, key string) (already bool, err error) {
	data, err := c.onboardingCall(ctx, accountID, session, onboardingComplete, http.MethodPost, map[string]any{"key": key})
	if err != nil {
		return false, err
	}
	already, _ = data["alreadyCompleted"].(bool)
	return already, nil
}

// sessionForClaim loads a usable session, logging in again when needed.
func (c *Client) sessionForClaim(ctx context.Context, accountID string) (string, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return "", err
	}
	return cred.Session, nil
}

// PreviewClaim lists the eight tasks plus (when anything is outstanding) a
// one-click plan that claims them all.
func (c *Client) PreviewClaim(ctx context.Context, accountID string) ([]providers.ClaimPlan, error) {
	session, err := c.sessionForClaim(ctx, accountID)
	if err != nil {
		return nil, err
	}
	done, err := c.onboardingState(ctx, accountID, session)
	if err != nil {
		return nil, err
	}

	plans := make([]providers.ClaimPlan, 0, len(onboardingTasks)+1)
	outstanding := 0
	for _, task := range onboardingTasks {
		status := "available"
		if done[task.Key] {
			status = "claimed"
		} else {
			outstanding++
		}
		plans = append(plans, providers.ClaimPlan{
			PlanID: taskPlanPrefix + task.Key,
			Name:   fmt.Sprintf("%s（%d 积分）", task.Title, task.Points),
			Status: status,
		})
	}
	if outstanding > 0 {
		plans = append([]providers.ClaimPlan{{
			PlanID: claimAllPlanID,
			Name:   fmt.Sprintf("一键领取全部新手任务（%d 项未领，最多 10000 积分）", outstanding),
			Status: "available",
		}}, plans...)
	}
	return plans, nil
}

// Claim claims one task, or every outstanding task for the one-click plan.
func (c *Client) Claim(ctx context.Context, accountID, planID string) (providers.ClaimResult, error) {
	session, err := c.sessionForClaim(ctx, accountID)
	if err != nil {
		return providers.ClaimResult{}, err
	}
	planID = strings.TrimSpace(planID)

	if planID == claimAllPlanID {
		return c.claimAll(ctx, accountID, session)
	}
	key := strings.TrimPrefix(planID, taskPlanPrefix)
	task, known := onboardingTaskByKey(key)
	if !known {
		return providers.ClaimResult{}, fmt.Errorf("loomy 没有名为 %q 的新手任务", planID)
	}
	already, err := c.claimTask(ctx, accountID, session, key)
	if err != nil {
		if reloginErr := c.retryClaimAfterAuth(ctx, accountID, err, func(next string) error {
			var claimErr error
			already, claimErr = c.claimTask(ctx, accountID, next, key)
			return claimErr
		}); reloginErr != nil {
			return providers.ClaimResult{}, reloginErr
		}
	}
	if already {
		return providers.ClaimResult{
			AlreadyClaimed: true,
			PlanID:         planID,
			Message:        fmt.Sprintf("%s：上游 alreadyCompleted（已领过）", task.Title),
		}, nil
	}
	return providers.ClaimResult{
		Claimed: true,
		PlanID:  planID,
		Message: fmt.Sprintf("%s：+%d 积分", task.Title, task.Points),
	}, nil
}

// claimAll walks the table serially, skipping what is already done — the same
// order the official client uses.
func (c *Client) claimAll(ctx context.Context, accountID, session string) (providers.ClaimResult, error) {
	done, err := c.onboardingState(ctx, accountID, session)
	if err != nil {
		return providers.ClaimResult{}, err
	}
	claimed := 0
	points := 0
	skipped := 0
	failed := []string{}
	for _, task := range onboardingTasks {
		if done[task.Key] {
			skipped++
			continue
		}
		already, claimErr := c.claimTask(ctx, accountID, session, task.Key)
		if claimErr != nil {
			if providerErr, ok := claimErr.(*providers.Error); ok && providerErr.Kind == "auth" {
				// One relogin, then keep going: the session may simply have
				// aged out between the preview and this call.
				cred, reloginErr := c.relogin(ctx, accountID)
				if reloginErr != nil {
					return providers.ClaimResult{}, reloginErr
				}
				session = cred.Session
				if already, claimErr = c.claimTask(ctx, accountID, session, task.Key); claimErr == nil {
					if already {
						skipped++
						continue
					}
					claimed++
					points += task.Points
					continue
				}
			}
			failed = append(failed, task.Key)
			continue
		}
		if already {
			skipped++
			continue
		}
		claimed++
		points += task.Points
	}

	message := fmt.Sprintf("本次领取 %d 项，+%d 积分；已领过 %d 项", claimed, points, skipped)
	if len(failed) > 0 {
		message += fmt.Sprintf("；失败 %d 项（%s）", len(failed), strings.Join(failed, ", "))
	}
	return providers.ClaimResult{
		Claimed: claimed > 0,
		PlanID:  claimAllPlanID,
		Message: message,
	}, nil
}

// retryClaimAfterAuth re-runs a claim once with a fresh session when the first
// attempt failed on auth.
func (c *Client) retryClaimAfterAuth(ctx context.Context, accountID string, cause error, retry func(session string) error) error {
	providerErr, ok := cause.(*providers.Error)
	if !ok || providerErr.Kind != "auth" {
		return cause
	}
	cred, err := c.relogin(ctx, accountID)
	if err != nil {
		return err
	}
	return retry(cred.Session)
}
