package zcode

import (
	_ "embed"
	"encoding/json"
	"strings"
	"time"
)

// The plan gateway performs its own content review. A Start Plan request is
// only accepted when its system prompt is shaped the way the ZCode desktop
// client shapes it: exactly three cached blocks, the first two byte for byte
// the client's, every dynamic section (the client's before/after sections, the
// environment, and the caller's own prompt) joined with "\n\n" into the third.
// A fourth block is refused with 405/3012 "unusual activity" (magpie
// plugins#343), so the block count is a contract, not a style choice.
//
// The texts below are ZCode 3.14.3's, the same ones zcode.z.ai checks for.
//
//go:embed zcode_prompt.json
var zcodePromptJSON []byte

// zcodePrompt is the client's own prompt set.
type zcodePrompt struct {
	CLIPrefix         string   `json:"cliPrefix"`
	StableSections    []string `json:"stableSections"`
	BeforeEnvironment string   `json:"beforeEnvironment"`
	AfterEnvironment  string   `json:"afterEnvironment"`
	Environment       struct {
		Heading       string `json:"heading"`
		InvokedLine   string `json:"invokedLine"`
		PoweredByLine string `json:"poweredByLine"`
	} `json:"environment"`
	// Context is the day reminder the client puts in a user turn ahead of the
	// agent's own first turn, wrapped in <system-reminder> tags.
	Context struct {
		Intro              string `json:"intro"`
		Outro              string `json:"outro"`
		CurrentDateHeading string `json:"currentDateHeading"`
		CurrentDateLine    string `json:"currentDateLine"`
	} `json:"context"`
}

var zcodePlanPrompt = func() *zcodePrompt {
	var p zcodePrompt
	if err := json.Unmarshal(zcodePromptJSON, &p); err != nil {
		return nil
	}
	if p.CLIPrefix == "" || len(p.StableSections) == 0 {
		return nil
	}
	return &p
}()

// applyPlanIdentity reshapes the body's system prompt into the three-block form
// the desktop client sends, puts the day reminder ahead of the conversation,
// stamps the resolved model into the environment section and the account
// identity into metadata.user_id.
//
// It returns true when the body was rewritten. A body that already carries the
// official first block is left alone, which keeps the call idempotent.
func applyPlanIdentity(body map[string]any, model, userID string) bool {
	prompt := zcodePlanPrompt
	if prompt == nil || body == nil {
		return false
	}
	if first := firstSystemText(body["system"]); first != "" && first == prompt.CLIPrefix {
		return false
	}

	dynamic := make([]string, 0, 4)
	dynamic = append(dynamic, prompt.BeforeEnvironment)
	dynamic = append(dynamic, planEnvironment(prompt, model))
	dynamic = append(dynamic, prompt.AfterEnvironment)
	if user := userSystemText(body["system"]); user != "" {
		dynamic = append(dynamic, user)
	}

	body["system"] = []any{
		planSystemBlock(prompt.CLIPrefix),
		planSystemBlock(strings.Join(prompt.StableSections, "\n\n")),
		planSystemBlock("\n\n" + strings.Join(dynamic, "\n\n")),
	}

	prependContextPrefix(body, prompt)
	finalizeCacheControl(body)

	if id := strings.TrimSpace(userID); id != "" {
		metadata, _ := body["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["user_id"] = planUserID(id)
		body["metadata"] = metadata
	}
	return true
}

func planSystemBlock(text string) map[string]any {
	return map[string]any{
		"type":          "text",
		"text":          text,
		"cache_control": map[string]any{"type": "ephemeral"},
	}
}

// planEnvironment rebuilds the client's "# Environment" section. Platform and
// OS version come from the same identity settings the headers use, so the body
// and the headers never disagree — a mismatch is itself a fingerprint signal.
func planEnvironment(prompt *zcodePrompt, model string) string {
	platform := envOr("ZCODE_IDENTITY_PLATFORM", identityPlatform)
	osVersion := envOr("ZCODE_IDENTITY_OS_VERSION", identityOSVersion)
	workingDir := envOr("ZCODE_WORKING_DIR", "unknown")
	lines := []string{
		prompt.Environment.Heading,
		prompt.Environment.InvokedLine,
		"- Primary working directory: " + workingDir,
		"- Platform: " + platform,
		"- OS Version: " + platform + " " + osVersion,
	}
	if m := strings.TrimSpace(model); m != "" {
		line := strings.ReplaceAll(prompt.Environment.PoweredByLine, "{provider}", "zai-api")
		lines = append(lines, strings.ReplaceAll(line, "{model}", m))
	}
	return strings.Join(lines, "\n")
}

// planUserID renders metadata.user_id the way the client does: a JSON object
// encoded as a string, with the field order the gateway expects.
func planUserID(device string) string {
	encoded, err := json.Marshal(struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}{DeviceID: device})
	if err != nil {
		return device
	}
	return string(encoded)
}

// userSystemText returns the caller's own system prompt as plain text.
func userSystemText(system any) string {
	switch typed := system.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		var parts []string
		for _, item := range typed {
			if block, ok := item.(map[string]any); ok {
				if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

func firstSystemText(system any) string {
	switch typed := system.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		if len(typed) == 0 {
			return ""
		}
		if block, ok := typed[0].(map[string]any); ok {
			if text, ok := block["text"].(string); ok {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

// prependContextPrefix puts the day reminder in a user turn ahead of the
// caller's first turn, the way the desktop client opens a conversation: the
// text wrapped in <system-reminder> tags, the sections joined by "\n". It is
// skipped when the body already opens with one, so a retried or re-dressed
// request keeps exactly one.
func prependContextPrefix(body map[string]any, prompt *zcodePrompt) {
	messages, ok := body["messages"].([]map[string]any)
	if !ok || len(messages) == 0 {
		return
	}
	if role, _ := messages[0]["role"].(string); role == "user" {
		if hasSystemReminder(messages[0]) {
			return
		}
	}
	context := prompt.Context
	if context.Intro == "" || context.CurrentDateLine == "" {
		return
	}
	line := strings.ReplaceAll(context.CurrentDateLine, "{date}", today())
	parts := []string{context.Intro, context.CurrentDateHeading + "\n" + line, "", context.Outro}
	text := "<system-reminder>" + strings.Join(parts, "\n") + "</system-reminder>"
	body["messages"] = append([]map[string]any{{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": text}},
	}}, messages...)
}

// hasSystemReminder reports whether a message's first text block is already the
// client's day reminder.
func hasSystemReminder(message map[string]any) bool {
	content, ok := message["content"].([]any)
	if !ok || len(content) == 0 {
		return false
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		return false
	}
	text, _ := block["text"].(string)
	return strings.HasPrefix(text, "<system-reminder>")
}

// today is the reminder's date in the client's format. A fixed clock would make
// the reminder stale, so it is read per request.
func today() string {
	return time.Now().Format("2006-01-02")
}

// finalizeCacheControl reproduces the client's cache marks: none on the turns
// and none on the tools, then one ephemeral mark on the last block of the last
// non-system turn. Marks the caller brought are removed first — the gateway
// compares the cache layout, and stray markers are not part of it.
func finalizeCacheControl(body map[string]any) {
	if tools, ok := body["tools"].([]map[string]any); ok {
		for _, tool := range tools {
			delete(tool, "cache_control")
		}
	}
	messages, ok := body["messages"].([]map[string]any)
	if !ok || len(messages) == 0 {
		return
	}
	var last map[string]any
	for _, message := range messages {
		if role, _ := message["role"].(string); role == "system" {
			continue
		}
		last = message
		if content, ok := message["content"].([]any); ok {
			for _, item := range content {
				if block, ok := item.(map[string]any); ok {
					delete(block, "cache_control")
				}
			}
		}
	}
	if last == nil {
		return
	}
	switch content := last["content"].(type) {
	case string:
		last["content"] = []any{planSystemBlock(content)}
	case []any:
		if len(content) == 0 {
			return
		}
		if block, ok := content[len(content)-1].(map[string]any); ok {
			block["cache_control"] = map[string]any{"type": "ephemeral"}
		}
	}
}
