package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// ModelSettingReader is optional. Missing it means the console reasoning
// default is skipped and the catalog default is used.
type ModelSettingReader interface {
	GetProviderModelSetting(context.Context, string, string) (accounts.ProviderModelSetting, error)
}

// buildChatBody serializes the internal chat request for Gemini's
// OpenAI-compatible endpoint. reasoning_effort is written only for gemini-*
// models; Claude and GPT-OSS models keep their upstream thinking behavior.
func buildChatBody(req translate.ChatRequest, model string, level string) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return nil, err
	}
	obj["model"] = model
	obj["stream"] = true
	applyReasoning(obj, model, level)
	return json.Marshal(obj)
}

// applyReasoning writes the clamped level as a flat reasoning_effort for
// gemini-* models. "none" is omitted: the OpenAI-compat layer has no disable
// switch for the thinking scale, and sending one would invent a level the
// catalog does not declare.
func applyReasoning(obj map[string]any, model, level string) {
	delete(obj, "reasoning")
	if !isGeminiModel(model) || level == "" || level == "none" {
		delete(obj, "reasoning_effort")
		return
	}
	obj["reasoning_effort"] = geminiEffort(level)
}

// geminiEffort maps the gateway vocabulary onto the Gemini thinking scale.
// The OpenAI-compat layer accepts low/medium/high and translates them to the
// model's thinking_level. xhigh/max collapse to high; anything unknown falls
// back to medium.
func geminiEffort(level string) string {
	switch providers.NormalizeReasoningLevel(level) {
	case "low":
		return "low"
	case "high", "xhigh", "max":
		return "high"
	default:
		return "medium"
	}
}

func isGeminiModel(model string) bool {
	return strings.HasPrefix(canonicalModel(model), GeminiPrefix)
}

func canonicalModel(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// reasoningCaps declares the levels this adapter will send. Only gemini-*
// models carry the OpenAI effort scale in this adapter.
func reasoningCaps(model string) providers.ModelCapabilities {
	if !isGeminiModel(model) {
		return providers.ModelCapabilities{Tools: true}
	}
	return providers.ModelCapabilities{
		Tools:              true,
		Reasoning:          true,
		ReasoningOptions:   []string{"low", "medium", "high"},
		ReasoningDefault:   "medium",
		ReasoningType:      "effort",
		CanDisableThinking: false,
	}
}

// storedReasoning returns the console reasoning default for a model, empty
// when the store does not expose model settings.
func (c *Client) storedReasoning(ctx context.Context, model string) string {
	reader, ok := c.store.(ModelSettingReader)
	if !ok {
		return ""
	}
	stored, err := reader.GetProviderModelSetting(ctx, "antigravity", accounts.CanonicalModelID(model))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(stored.ReasoningEffort)
}
