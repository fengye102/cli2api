package zcode

import (
	_ "embed"
	"encoding/json"
	"strings"
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
// the desktop client sends, stamps the resolved model into the environment
// section and the account identity into metadata.user_id.
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

// finalizeCacheControl marks the last non-system content block as ephemeral,
// mirroring the desktop client. Anthropic silently ignores the marker on
// requests below the caching threshold, so it is safe to always add.
func finalizeCacheControl(body map[string]any) {
	messages, ok := body["messages"].([]map[string]any)
	if !ok || len(messages) == 0 {
		return
	}
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if role, _ := message["role"].(string); role == "system" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			message["content"] = []any{planSystemBlock(content)}
		case []any:
			if len(content) == 0 {
				return
			}
			if last, ok := content[len(content)-1].(map[string]any); ok {
				if _, exists := last["cache_control"]; !exists {
					last["cache_control"] = map[string]any{"type": "ephemeral"}
				}
			}
		}
		return
	}
}
