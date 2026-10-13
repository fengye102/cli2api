package antigravity

import (
	"encoding/json"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// v1internal model catalog: `POST {base}:fetchAvailableModels`.
//
// The upstream answers with an object keyed by model id (not an array):
//
//	{"models": {"<id>": {quotaInfo, displayName, supportsImages,
//	                     supportsThinking, thinkingBudget, recommended,
//	                     maxTokens, maxOutputTokens, supportedMimeTypes}},
//	 "deprecatedModelIds": {"<old>": {"newModelId": "<new>"}}}
//
// Only the Gemini family is advertised. The endpoint also serves Claude and
// GPT-OSS checkpoints, but this adapter does not implement them, so a prefix
// allow-list keeps an unimplemented channel from being advertised at all —
// the safer default when upstream adds another family later.
//
// The ids upstream publishes are already upstream-native names; the alias table
// below only rewrites the friendly ids this adapter's fallback list uses.

// upstreamModelAliases maps a public id to the name the v1internal endpoint
// expects. Ids absent from the table pass through unchanged, which is correct
// for anything the remote catalog itself published.
var upstreamModelAliases = map[string]string{
	"gemini-3.6-flash":      "gemini-3.6-flash-tiered",
	"gemini-3.7-flash":      "gemini-3.7-flash-tiered",
	"gemini-3.8-flash":      "gemini-3.8-flash-tiered",
	"gemini-3.1-pro-high":   "gemini-pro-agent",
	"gemini-3-pro-high":     "gemini-pro-agent",
	"gemini-2.5-flash-lite": "gemini-2.5-flash",
}

// upstreamModelID resolves a public id to the name v1internal expects.
func upstreamModelID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return ""
	}
	if mapped, ok := upstreamModelAliases[strings.ToLower(trimmed)]; ok {
		return mapped
	}
	// A bare `gemini-<x.y>-flash` at 3.6 or above takes the adaptive (-tiered)
	// variant; anything already carrying a variant suffix is left alone.
	if tiered, ok := tieredFlashID(trimmed); ok {
		return tiered
	}
	return trimmed
}

// tieredFlashID appends -tiered to a bare Flash id whose version is >= 3.6.
func tieredFlashID(id string) (string, bool) {
	lowered := strings.ToLower(id)
	for _, suffix := range []string{
		"-high", "-medium", "-low", "-extra-low", "-tiered", "-preview",
		"-agent", "-thinking", "-image",
	} {
		if strings.Contains(lowered, suffix) {
			return "", false
		}
	}
	if !strings.Contains(lowered, "flash") {
		return "", false
	}
	rest, ok := strings.CutPrefix(lowered, "gemini-")
	if !ok {
		return "", false
	}
	version := ""
	for _, ch := range rest {
		if (ch >= '0' && ch <= '9') || ch == '.' {
			version += string(ch)
			continue
		}
		break
	}
	version = strings.TrimSuffix(version, ".")
	if version == "" {
		return "", false
	}
	major, minor := parseMajorMinor(version)
	if major < 3 || (major == 3 && minor < 6) {
		return "", false
	}
	return id + "-tiered", true
}

// parseMajorMinor reads the leading two numeric segments of a version string.
func parseMajorMinor(version string) (int, int) {
	parts := strings.Split(version, ".")
	major := atoiSafe(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor = atoiSafe(parts[1])
	}
	return major, minor
}

// atoiSafe parses digits, yielding 0 on anything else.
func atoiSafe(text string) int {
	value := 0
	for _, ch := range strings.TrimSpace(text) {
		if ch < '0' || ch > '9' {
			return 0
		}
		value = value*10 + int(ch-'0')
	}
	return value
}

// parseV1Catalog maps a fetchAvailableModels body onto ModelInfo values.
func parseV1Catalog(raw []byte) []providers.ModelInfo {
	var payload struct {
		Models map[string]map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	models := make([]providers.ModelInfo, 0, len(payload.Models))
	for id, entry := range payload.Models {
		trimmed := strings.TrimSpace(id)
		if !isGeminiFamily(trimmed) {
			continue
		}
		models = append(models, v1EntryInfo(trimmed, entry))
	}
	return models
}

// isGeminiFamily reports whether the id belongs to the Gemini family.
func isGeminiFamily(id string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), "gemini-")
}

// v1EntryInfo maps one catalog entry onto ModelInfo.
func v1EntryInfo(id string, entry map[string]any) providers.ModelInfo {
	displayName := strings.TrimSpace(stringValueOf(entry["displayName"]))
	if displayName == "" {
		displayName = id
	}
	contextWindow := int(intValueOf(entry["maxTokens"]))
	maxOutput := int(intValueOf(entry["maxOutputTokens"]))
	if maxOutput <= 0 {
		maxOutput = DefaultMaxTokens
	}
	thinking := false
	if value, ok := entry["supportsThinking"].(bool); ok {
		thinking = value
	}
	images := true
	if value, ok := entry["supportsImages"].(bool); ok {
		images = value
	}
	caps := providers.ModelCapabilities{
		ContextWindow: contextWindow,
		MaxOutput:     maxOutput,
		Tools:         true,
		Images:        images,
		Reasoning:     thinking,
	}
	if thinking {
		caps.ReasoningOptions = []string{"low", "medium", "high"}
		caps.ReasoningDefault = "medium"
		caps.ReasoningType = "effort"
		caps.CanDisableThinking = false
	}
	return providers.ModelInfo{
		NativeModel:  id,
		PublicModel:  id,
		DisplayName:  displayName,
		Capabilities: caps,
	}
}
