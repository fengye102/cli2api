package zcode

import (
	"crypto/sha256"
	"encoding/hex"
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
	// Exactly three blocks: a fourth is refused upstream with 405/3012.
	blocks, ok := body["system"].([]any)
	if !ok || len(blocks) != 3 {
		t.Fatalf("system block count=%d want 3 (%#v)", len(blocks), body["system"])
	}
	first, _ := blocks[0].(map[string]any)
	if first["text"] != zcodePlanPrompt.CLIPrefix {
		t.Errorf("first block=%v", first["text"])
	}
	if first["text"] != "You are ZCode, an interactive coding agent" {
		t.Errorf("first block must stay the client's byte for byte: %v", first["text"])
	}
	second, _ := blocks[1].(map[string]any)
	if second["text"] != strings.Join(zcodePlanPrompt.StableSections, "\n\n") {
		t.Error("second block must be the client's stable sections joined by \\n\\n")
	}
	// Everything dynamic belongs to the third block, in order, joined by \n\n.
	third, _ := blocks[2].(map[string]any)
	text, _ := third["text"].(string)
	if !strings.HasPrefix(text, "\n\n") {
		t.Errorf("third block must start with \\n\\n, got %q", text[:min(12, len(text))])
	}
	for _, want := range []string{
		zcodePlanPrompt.BeforeEnvironment,
		"# Environment",
		"- Platform: " + identityPlatform,
		"- OS Version: " + identityPlatform + " " + identityOSVersion,
		"GLM-5.3-Flash",
		zcodePlanPrompt.AfterEnvironment,
		"be terse",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("third block missing %q", want)
		}
	}
	// The caller's prompt must come last so the sections stay in client order.
	if strings.LastIndex(text, "be terse") < strings.LastIndex(text, zcodePlanPrompt.AfterEnvironment) {
		t.Error("caller system prompt must follow the client's afterEnvironment section")
	}
	metadata, _ := body["metadata"].(map[string]any)
	// Field order is part of the contract: device_id, account_uuid, session_id.
	want := `{"device_id":"user-1","account_uuid":"","session_id":""}`
	if metadata["user_id"] != want {
		t.Errorf("metadata.user_id=%v want %s", metadata["user_id"], want)
	}
	// The day reminder opens the conversation as its own user turn.
	messages, _ := body["messages"].([]map[string]any)
	if len(messages) != 2 {
		t.Fatalf("message count=%d want 2 (reminder + caller turn)", len(messages))
	}
	reminder, _ := messages[0]["content"].([]any)
	if len(reminder) != 1 {
		t.Fatalf("reminder blocks=%#v", messages[0]["content"])
	}
	reminderBlock, _ := reminder[0].(map[string]any)
	reminderText, _ := reminderBlock["text"].(string)
	if !strings.HasPrefix(reminderText, "<system-reminder>") || !strings.HasSuffix(reminderText, "</system-reminder>") {
		t.Errorf("reminder must be wrapped in system-reminder tags: %q", reminderText)
	}
	if !strings.Contains(reminderText, zcodePlanPrompt.Context.CurrentDateHeading+"\n") {
		t.Error("reminder must carry the currentDate heading on its own line")
	}
	if !strings.Contains(reminderText, "Today's date is ") || strings.Contains(reminderText, "{date}") {
		t.Errorf("reminder must carry a substituted date: %q", reminderText)
	}
	// The cache mark belongs to the last block of the last turn, not the reminder.
	if _, marked := reminderBlock["cache_control"]; marked {
		t.Error("the reminder turn must not carry cache_control")
	}
	last, _ := messages[len(messages)-1]["content"].([]any)
	if len(last) != 1 {
		t.Fatalf("cache control rewrite=%#v", messages[len(messages)-1]["content"])
	}
	block, _ := last[0].(map[string]any)
	if _, ok := block["cache_control"].(map[string]any); !ok {
		t.Error("last content block of the last turn must carry cache_control")
	}
	// Idempotent: a body that already carries the official prefix is untouched.
	if applyPlanIdentity(body, "GLM-5.3-Flash", "user-1") {
		t.Error("second call must be a no-op")
	}
	if count := countSystemReminders(body); count != 1 {
		t.Errorf("system-reminder turns=%d want 1", count)
	}
}

// countSystemReminders counts the day-reminder turns in a body.
func countSystemReminders(body map[string]any) int {
	messages, _ := body["messages"].([]map[string]any)
	total := 0
	for _, message := range messages {
		if hasSystemReminder(message) {
			total++
		}
	}
	return total
}

// The caller's own cache marks and tool marks are not part of the client's
// layout, so they must be stripped before the single one is set.
func TestFinalizeCacheControlStripsCallerMarks(t *testing.T) {
	mark := map[string]any{"type": "ephemeral"}
	body := map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "first", "cache_control": mark},
			}},
			{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "second", "cache_control": mark},
			}},
			{"role": "user", "content": "third"},
		},
		"tools": []map[string]any{
			{"name": "read", "cache_control": mark},
		},
	}
	finalizeCacheControl(body)

	messages := body["messages"].([]map[string]any)
	first := messages[0]["content"].([]any)[0].(map[string]any)
	if _, exists := first["cache_control"]; exists {
		t.Error("caller mark on an earlier turn must be removed")
	}
	second := messages[1]["content"].([]any)[0].(map[string]any)
	if _, exists := second["cache_control"]; exists {
		t.Error("caller mark on the assistant turn must be removed")
	}
	if _, exists := body["tools"].([]map[string]any)[0]["cache_control"]; exists {
		t.Error("tool cache_control must be removed")
	}
	last := messages[2]["content"].([]any)
	if len(last) != 1 {
		t.Fatalf("string content must become one block: %#v", last)
	}
	if _, ok := last[0].(map[string]any)["cache_control"].(map[string]any); !ok {
		t.Error("the last block must carry exactly one cache_control")
	}
	// Exactly one marker survives in the whole body.
	total := 0
	for _, message := range messages {
		content, _ := message["content"].([]any)
		for _, item := range content {
			if block, ok := item.(map[string]any); ok {
				if _, marked := block["cache_control"]; marked {
					total++
				}
			}
		}
	}
	if total != 1 {
		t.Errorf("cache_control count=%d want 1", total)
	}
}

// The three blocks are a contract with the gateway: pin the digests magpie
// documents so an edit to the embedded prompt cannot silently break it.
func TestPlanPromptMatchesClientDigests(t *testing.T) {
	if zcodePlanPrompt == nil {
		t.Fatal("embedded prompt failed to load")
	}
	digest := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:8])
	}
	for name, want := range map[string]struct{ text, digest string }{
		"cliPrefix":         {zcodePlanPrompt.CLIPrefix, "46dd360a22c87a92"},
		"stableSections[0]": {zcodePlanPrompt.StableSections[0], "3f21ff9a88a03a76"},
		"stableSections[1]": {zcodePlanPrompt.StableSections[1], "39a1e86c93452c01"},
		"beforeEnvironment": {zcodePlanPrompt.BeforeEnvironment, "bbdc66b399d297ff"},
		"afterEnvironment":  {zcodePlanPrompt.AfterEnvironment, "1732b7f098a925d7"},
	} {
		if got := digest(want.text); got != want.digest {
			t.Errorf("%s digest=%s want %s (text no longer matches the client)", name, got, want.digest)
		}
	}
	if len(zcodePlanPrompt.StableSections) != 2 {
		t.Errorf("stableSections count=%d want 2", len(zcodePlanPrompt.StableSections))
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
