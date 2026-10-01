package zcode

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// The plan gateway performs its own content review: a request whose system
// prompt lacks the official ZCode identity blocks is rejected with 3012
// "unusual activity" no matter how valid the credential is. The blocks are the
// desktop client's own prefix, extracted verbatim from its bundle.
//
//go:embed zcode_system.json
var zcodeSystemJSON []byte

var zcodeSystemBlocks = func() []map[string]any {
	var blocks []map[string]any
	if err := json.Unmarshal(zcodeSystemJSON, &blocks); err != nil {
		return nil
	}
	return blocks
}()

// applyPlanIdentity prepends the official identity blocks, stamps the resolved
// model, appends the caller's own system prompt and adds the cache markers the
// desktop client sends. It is idempotent: a body already carrying the first
// official block is left alone.
func applyPlanIdentity(body map[string]any, model, userID string) bool {
	if len(zcodeSystemBlocks) == 0 || body == nil {
		return false
	}
	if first := firstSystemText(body["system"]); first != "" && first == systemBlockText(zcodeSystemBlocks[0]) {
		return false
	}
	system := make([]any, 0, len(zcodeSystemBlocks)+2)
	for _, block := range zcodeSystemBlocks {
		cloned := make(map[string]any, len(block))
		for k, v := range block {
			cloned[k] = v
		}
		system = append(system, cloned)
	}
	if m := strings.TrimSpace(model); m != "" {
		system = append(system, map[string]any{
			"type":          "text",
			"text":          "- You are powered by the model named " + m + ".",
			"cache_control": map[string]any{"type": "ephemeral"},
		})
	}
	if user := userSystemText(body["system"]); user != "" {
		system = append(system, map[string]any{"type": "text", "text": user})
	}
	body["system"] = system

	finalizeCacheControl(body)

	if id := strings.TrimSpace(userID); id != "" {
		metadata, _ := body["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["user_id"] = id
		body["metadata"] = metadata
	}
	return true
}

func systemBlockText(block map[string]any) string {
	text, _ := block["text"].(string)
	return text
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
			return strings.TrimSpace(systemBlockText(block))
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
			message["content"] = []any{map[string]any{
				"type":          "text",
				"text":          content,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
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
