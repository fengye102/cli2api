package zcode

import (
	"os"
	"strings"
	"testing"
)

func TestDeviceMidIsStablePerAccount(t *testing.T) {
	os.Unsetenv("ZCODE_DEVICE_MID")
	first := deviceMid("acc_4e52cfae87d3")
	if first != deviceMid("acc_4e52cfae87d3") {
		t.Error("device mid must be stable for one account")
	}
	if first == deviceMid("acc_other") {
		t.Error("different accounts must not share a device mid")
	}
	if len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Errorf("device mid is not UUID-shaped: %q", first)
	}
	if got := deviceMid(""); got != "" {
		t.Errorf("empty account id must yield no device mid, got %q", got)
	}
}

func TestIdentityHeadersCarryDesktopShape(t *testing.T) {
	headers := identityHeaders("acc_1")
	for key, want := range map[string]string{
		"X-Title":             "Z Code@electron",
		"X-Release-Channel":   "stable",
		"X-Client-Language":   "zh-CN",
		"X-Client-Timezone":   "Asia/Shanghai",
		"X-Platform":          "darwin-arm64",
		"X-Os-Category":       "macos",
		"X-ZCode-Agent":       "glm",
		"HTTP-Referer":        "https://zcode.z.ai/",
		"X-ZCode-App-Version": Version,
	} {
		if headers[key] != want {
			t.Errorf("%s=%q want %q", key, headers[key], want)
		}
	}
	if !strings.HasPrefix(headers["User-Agent"], "ZCode/") {
		t.Errorf("User-Agent=%q", headers["User-Agent"])
	}
	if headers["X-Device-Mid"] == "" {
		t.Error("X-Device-Mid missing")
	}
	// The coding-plan trace ids must never leak into a plan request.
	trace := traceHeaders()
	if trace["x-request-id"] == "" || trace["x-zcode-trace-id"] == "" || trace["x-zcode-session-type"] != "main" {
		t.Errorf("trace headers=%v", trace)
	}
	if _, exists := trace["x-session-id"]; exists {
		t.Error("plan channel must not send x-session-id")
	}
}

func TestApplyPlanIdentity(t *testing.T) {
	body := map[string]any{
		"model":    "GLM-5.3-Flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   "be terse",
	}
	if !applyPlanIdentity(body, "GLM-5.3-Flash", "user-1") {
		t.Fatal("identity blocks were not injected")
	}
	blocks, ok := body["system"].([]any)
	if !ok || len(blocks) < 3 {
		t.Fatalf("system blocks=%#v", body["system"])
	}
	first, _ := blocks[0].(map[string]any)
	if first["text"] != "You are ZCode, an interactive coding agent" {
		t.Errorf("first block=%v", first["text"])
	}
	model, _ := blocks[len(blocks)-2].(map[string]any)
	if !strings.Contains(model["text"].(string), "GLM-5.3-Flash") {
		t.Errorf("model block=%v", model["text"])
	}
	user, _ := blocks[len(blocks)-1].(map[string]any)
	if user["text"] != "be terse" {
		t.Errorf("user system prompt lost: %v", user["text"])
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata["user_id"] != "user-1" {
		t.Errorf("metadata=%v", body["metadata"])
	}
	messages, _ := body["messages"].([]map[string]any)
	content, _ := messages[0]["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("cache control rewrite=%#v", messages[0]["content"])
	}
	block, _ := content[0].(map[string]any)
	if _, ok := block["cache_control"].(map[string]any); !ok {
		t.Error("last content block must carry cache_control")
	}
	// Idempotent: a body that already carries the official prefix is untouched.
	if applyPlanIdentity(body, "GLM-5.3-Flash", "user-1") {
		t.Error("second call must be a no-op")
	}
}

func TestParseVerifyParam(t *testing.T) {
	out := "[pw] launch chromium\nVERIFY_PARAM=eyJjZXR0\n"
	if got := parseVerifyParam(out); got != "eyJjZXR0" {
		t.Errorf("param=%q", got)
	}
	if got := parseVerifyParam("no token here\n"); got != "" {
		t.Errorf("param=%q want empty", got)
	}
}
