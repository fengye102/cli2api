package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// cloudaicompanionProject discovery: loadCodeAssist, then onboardUser when the
// account has no project yet.
//
// The chat request body carries "project": "<cloudaicompanionProject>", a value
// that is per Google account and is not inside any token — it has to be asked
// for. The catalog, quota and loadCodeAssist calls themselves ignore the field,
// so a discovery failure never blocks adding an account or refreshing models.
//
// Both calls are pinned to prod: the daily host rejects these auth/onboarding
// requests (a documented quirk of the upstream).

// defaultTier is the fallback when allowedTiers carries no default.
const defaultTier = "legacy-tier"

// projectMetadata is the metadata object both calls send.
func projectMetadata() map[string]any {
	return map[string]any{"ideType": "ANTIGRAVITY"}
}

// projectOf reads cloudaicompanionProject from a loadCodeAssist response. Both
// shapes are accepted: a bare string, or an object carrying id.
func projectOf(payload map[string]any) string {
	return readProjectValue(payload["cloudaicompanionProject"])
}

// onboardProjectOf reads the same value from an onboardUser response, where it
// is nested under "response".
func onboardProjectOf(payload map[string]any) string {
	nested, ok := payload["response"].(map[string]any)
	if !ok {
		return ""
	}
	return readProjectValue(nested["cloudaicompanionProject"])
}

// readProjectValue resolves the two upstream shapes to a project id.
func readProjectValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		return strings.TrimSpace(stringValueOf(typed["id"]))
	default:
		return ""
	}
}

// defaultTierOf picks the allowedTiers entry with isDefault true, falling back
// to defaultTier.
func defaultTierOf(payload map[string]any) string {
	tiers, _ := payload["allowedTiers"].([]any)
	for _, entry := range tiers {
		tier, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		isDefault, _ := tier["isDefault"].(bool)
		if !isDefault {
			continue
		}
		if id := strings.TrimSpace(stringValueOf(tier["id"])); id != "" {
			return id
		}
	}
	return defaultTier
}

// postV1JSON sends one JSON POST to a v1internal method and returns status, the
// parsed body, and the raw text.
func (c *Client) postV1JSON(ctx context.Context, accountID, url, accessToken, projectID string, body map[string]any) (int, map[string]any, string, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, "", err
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return 0, nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, "", err
	}
	for key, value := range adminHeaders(accessToken, projectID) {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, "", &providers.Error{
			Kind:    "upstream",
			Status:  502,
			Message: "Antigravity 请求 Cloud Code Assist 失败：" + err.Error(),
		}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, decodeJSONObject(raw), string(raw), nil
}

// discoverProject runs loadCodeAssist and, when no project comes back, one
// onboardUser round.
//
// An error means "not obtained this time" (network, permission, or no
// entitlement); callers treat it as best-effort enrichment.
func (c *Client) discoverProject(ctx context.Context, accountID, accessToken string) (string, error) {
	if strings.TrimSpace(accessToken) == "" {
		return "", &providers.Error{Kind: "auth", Status: 401, Message: "Antigravity project 发现需要 access token"}
	}
	status, payload, raw, err := c.postV1JSON(ctx, accountID, loadCodeAssistURL(), accessToken, "", map[string]any{"metadata": projectMetadata()})
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", &providers.Error{
			Kind:    "upstream",
			Status:  502,
			Message: "Antigravity project 发现失败（HTTP " + itoa(status) + "）：" + upstreamMessage(payload, raw),
		}
	}
	if project := projectOf(payload); project != "" {
		return project, nil
	}
	return c.onboardProject(ctx, accountID, accessToken, defaultTierOf(payload))
}

// onboardProject runs one onboardUser round. done != true means "not onboarded
// yet" — the call is idempotent, so a later catalog refresh retries it.
func (c *Client) onboardProject(ctx context.Context, accountID, accessToken, tierID string) (string, error) {
	tier := strings.TrimSpace(tierID)
	if tier == "" {
		tier = defaultTier
	}
	body := map[string]any{"tierId": tier, "metadata": projectMetadata()}
	status, payload, raw, err := c.postV1JSON(ctx, accountID, onboardUserURL(), accessToken, "", body)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", &providers.Error{
			Kind:    "upstream",
			Status:  502,
			Message: "Antigravity project 开通失败（HTTP " + itoa(status) + "）：" + upstreamMessage(payload, raw),
		}
	}
	done, _ := payload["done"].(bool)
	project := onboardProjectOf(payload)
	if done && project != "" {
		return project, nil
	}
	message := "Antigravity 账号尚未完成开通（onboardUser 未返回 done，稍后刷新模型清单时会再试）"
	if done {
		message = "Antigravity project 开通响应里没有 cloudaicompanionProject（稍后刷新模型清单时会再试）"
	}
	return "", &providers.Error{Kind: "upstream", Status: 502, Message: message}
}

// upstreamMessage reads error.message, falling back to a truncated body.
func upstreamMessage(payload map[string]any, raw string) string {
	if nested, ok := payload["error"].(map[string]any); ok {
		if message := strings.TrimSpace(stringValueOf(nested["message"])); message != "" {
			return truncateText(message)
		}
	}
	if message := strings.TrimSpace(stringValueOf(payload["message"])); message != "" {
		return truncateText(message)
	}
	return truncateText(strings.TrimSpace(raw))
}

// itoa renders an int without importing strconv at every call site.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
