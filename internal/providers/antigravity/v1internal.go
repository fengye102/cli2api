package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// v1internal request building: chat envelope, Gemini contents, tool
// declarations and the ids the upstream expects.
//
// Two upstream rules shape the contents array: functionResponse turns must ride
// a user turn, and the list must both start and end with a user turn (a history
// ending on a model turn gets a continuation prompt appended).

const (
	// fallbackUserText appends a user turn when the history ends on a model turn.
	fallbackUserText = "Please continue your analysis."
	// leadingUserText pads a history that does not start with a user turn.
	leadingUserText = "..."
	// defaultThinkingBudget is used when thinking is requested without a level.
	defaultThinkingBudget = 32768
	// thinkingHeadroom keeps maxOutputTokens above the thinking budget.
	thinkingHeadroom = 8192
	// maxOutputTokensCap caps the completion budget upstream accepts.
	maxOutputTokensCap = 64000
)

// chatRequestBody builds the v1internal envelope for one chat request.
func chatRequestBody(req translate.ChatRequest, model, project string) ([]byte, error) {
	request := map[string]any{
		"contents":         contentsOf(req.Messages),
		"generationConfig": generationConfig(req),
		"sessionId":        sessionID(project, model, req.Messages),
	}
	if system := systemInstruction(req.Messages); system != nil {
		request["systemInstruction"] = system
	}
	if tools := functionDeclarations(req.Tools, req.ToolChoice); len(tools) > 0 {
		request["tools"] = []any{map[string]any{"functionDeclarations": tools}}
		request["toolConfig"] = map[string]any{
			"functionCallingConfig": map[string]any{"mode": "VALIDATED"},
		}
	}
	envelope := map[string]any{
		"model":     model,
		"userAgent": "antigravity",
		"requestId": requestID(),
		"request":   request,
	}
	if trimmed := strings.TrimSpace(project); trimmed != "" {
		envelope["project"] = trimmed
	}
	return json.Marshal(envelope)
}

// contentsOf converts chat messages into Gemini contents.
func contentsOf(messages []translate.ChatMessage) []any {
	names := map[string]string{}
	for _, message := range messages {
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			continue
		}
		for _, call := range decodeToolCalls(message.ToolCalls) {
			if call.ID != "" && call.Name != "" {
				names[call.ID] = call.Name
			}
		}
	}
	var contents []any
	for _, message := range messages {
		switch message.Role {
		case "system", "developer":
			continue
		case "tool":
			name := names[message.ToolCallID]
			if name == "" {
				name = strings.TrimSpace(message.Name)
			}
			if name == "" {
				name = "tool"
			}
			response := map[string]any{
				"name":     name,
				"response": map[string]any{"result": toolResultValue(translate.ContentToString(message.Content))},
			}
			if message.ToolCallID != "" {
				response["id"] = message.ToolCallID
			}
			// functionResponse must ride a user turn.
			contents = pushTurn(contents, "user", []any{map[string]any{"functionResponse": response}})
		case "assistant":
			parts := contentParts(message.Content)
			for _, call := range decodeToolCalls(message.ToolCalls) {
				if call.Name == "" {
					continue
				}
				body := map[string]any{"name": call.Name, "args": callArgs(call.Arguments)}
				if call.ID != "" {
					body["id"] = call.ID
				}
				parts = append(parts, map[string]any{"functionCall": body})
			}
			if len(parts) > 0 {
				contents = pushTurn(contents, "model", parts)
			}
		default:
			if parts := contentParts(message.Content); len(parts) > 0 {
				contents = pushTurn(contents, "user", parts)
			}
		}
	}
	if len(contents) > 0 {
		if roleOf(contents[len(contents)-1]) != "user" {
			contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{"text": fallbackUserText}}})
		}
		if roleOf(contents[0]) != "user" {
			contents = append([]any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": leadingUserText}}}}, contents...)
		}
	}
	return contents
}

// roleOf reads a content turn's role.
func roleOf(turn any) string {
	object, ok := turn.(map[string]any)
	if !ok {
		return ""
	}
	role, _ := object["role"].(string)
	return role
}

// pushTurn appends parts under a role, merging into the previous turn when the
// role repeats (the upstream expects alternating turns).
func pushTurn(contents []any, role string, parts []any) []any {
	if len(contents) > 0 {
		if last, ok := contents[len(contents)-1].(map[string]any); ok {
			if lastRole, _ := last["role"].(string); lastRole == role {
				existing, _ := last["parts"].([]any)
				last["parts"] = append(existing, parts...)
				return contents
			}
		}
	}
	return append(contents, map[string]any{"role": role, "parts": parts})
}

// contentParts converts message content (string or block array) into parts.
func contentParts(content any) []any {
	if text, ok := content.(string); ok {
		if text == "" {
			return nil
		}
		return []any{map[string]any{"text": text}}
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var parts []any
	for _, block := range blocks {
		if text, ok := block.(string); ok {
			if text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
			continue
		}
		object, ok := block.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := object["type"].(string)
		switch strings.ToLower(kind) {
		case "text", "input_text", "output_text":
			if text, _ := object["text"].(string); text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
		}
	}
	return parts
}

// systemInstruction folds system/developer messages into one instruction.
func systemInstruction(messages []translate.ChatMessage) map[string]any {
	var texts []string
	for _, message := range messages {
		if message.Role != "system" && message.Role != "developer" {
			continue
		}
		if text := strings.TrimSpace(translate.ContentToString(message.Content)); text != "" {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return map[string]any{
		"role":  "user",
		"parts": []any{map[string]any{"text": strings.Join(texts, "\n\n")}},
	}
}

// geminiToolCall is the decoded shape of one assistant tool call.
type geminiToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// decodeToolCalls reads the assistant tool_calls payload leniently.
func decodeToolCalls(raw json.RawMessage) []geminiToolCall {
	if len(raw) == 0 {
		return nil
	}
	var entries []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	out := make([]geminiToolCall, 0, len(entries))
	for _, entry := range entries {
		out = append(out, geminiToolCall{
			ID:        strings.TrimSpace(entry.ID),
			Name:      strings.TrimSpace(entry.Function.Name),
			Arguments: entry.Function.Arguments,
		})
	}
	return out
}

// callArgs normalizes tool arguments to the object Gemini requires.
func callArgs(raw json.RawMessage) map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]any{}
	}
	var object map[string]any
	if err := json.Unmarshal(trimmed, &object); err == nil {
		return object
	}
	// The arguments may arrive as a JSON string containing an object.
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		if err := json.Unmarshal([]byte(text), &object); err == nil {
			return object
		}
	}
	return map[string]any{}
}

// toolResultValue wraps a tool result so it is always an object, as Gemini
// requires.
func toolResultValue(text string) any {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return map[string]any{"result": ""}
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
		switch typed := parsed.(type) {
		case map[string]any:
			return typed
		case []any:
			return typed
		case nil:
			return map[string]any{"result": text}
		default:
			return map[string]any{"result": typed}
		}
	}
	return map[string]any{"result": text}
}

// generationConfig builds the sampling + thinking block.
func generationConfig(req translate.ChatRequest) map[string]any {
	config := map[string]any{"topK": 40, "topP": 1.0}
	if value := numberOf(req.Temperature); value != nil {
		config["temperature"] = value
	}
	if value := numberOf(req.TopP); value != nil {
		config["topP"] = value
	}
	max := intValue(req.MaxTokens)
	if max <= 0 {
		max = intValue(req.MaxCompletionTokens)
	}
	budget := thinkingBudgetOf(req)
	if budget > 0 {
		if max <= budget {
			max = budget + thinkingHeadroom
		}
		if max > maxOutputTokensCap {
			max = maxOutputTokensCap
		}
		// Invariant: maxOutputTokens must exceed the thinking budget. When the
		// caller's cap makes that impossible, shrink the budget instead.
		if max <= budget {
			budget = max - thinkingHeadroom
			if budget < 1 {
				budget = 1
			}
		}
		config["thinkingConfig"] = map[string]any{"includeThoughts": true, "thinkingBudget": budget}
		config["maxOutputTokens"] = max
	} else if max > 0 {
		if max > maxOutputTokensCap {
			max = maxOutputTokensCap
		}
		config["maxOutputTokens"] = max
	}
	return config
}

// thinkingBudgetOf maps the requested reasoning level onto a token budget.
func thinkingBudgetOf(req translate.ChatRequest) int {
	switch reasoningLevelOf(req.ReasoningEffort) {
	case "", "none":
		return 0
	case "low", "minimal", "extra-low":
		return 1000
	case "medium":
		return 4000
	default:
		return 10000
	}
}

// reasoningLevelOf reads the effort from the OpenAI-shaped reasoning_effort
// field (string or {effort|level|type}).
func reasoningLevelOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return providers.NormalizeReasoningLevel(typed)
	case map[string]any:
		for _, key := range []string{"effort", "level", "type"} {
			if text, ok := typed[key].(string); ok {
				if level := providers.NormalizeReasoningLevel(text); level != "" {
					return level
				}
			}
		}
	}
	return ""
}

// numberOf reads a JSON number leniently.
func numberOf(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil
	}
	switch typed := value.(type) {
	case float64:
		return typed
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%g", &parsed); err == nil {
			return parsed
		}
	}
	return nil
}

// intValue reads a JSON integer leniently.
func intValue(raw json.RawMessage) int {
	if value, ok := numberOf(raw).(float64); ok {
		return int(value)
	}
	return 0
}

// sessionID derives a stable session id from the conversation shape, so the
// upstream's server-side cache can recognize repeat turns of one session.
func sessionID(project, model string, messages []translate.ChatMessage) string {
	firstUser := ""
	systemText := ""
	for _, message := range messages {
		switch message.Role {
		case "system", "developer":
			if systemText == "" {
				systemText = translate.ContentToString(message.Content)
			}
		case "user":
			if firstUser == "" {
				firstUser = translate.ContentToString(message.Content)
			}
		}
	}
	return stableHash(project + "|" + model + "|" + systemText + "|" + firstUser)
}

// stableHash is an FNV-1a variant rendered as a decimal string, matching the
// shape the official client sends (a large negative integer).
func stableHash(text string) string {
	hash := int64(-3750763034362895579)
	for _, ch := range []byte(text) {
		hash *= 1099511628211
		hash ^= int64(ch)
	}
	return fmt.Sprintf("%d", hash)
}

// requestID is a per-request unique id: agent/<ms>/<8 hex>.
func requestID() string {
	millis := time.Now().UnixMilli()
	raw := randomBytes(4)
	if len(raw) == 0 {
		return fmt.Sprintf("agent/%d/%08x", millis, uint32(millis))
	}
	return fmt.Sprintf("agent/%d/%s", millis, hexString(raw))
}

// hexString renders bytes as lowercase hex.
func hexString(raw []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(raw)*2)
	for _, value := range raw {
		out = append(out, digits[value>>4], digits[value&0x0f])
	}
	return string(out)
}

// functionDeclarations converts the OpenAI tools payload into Gemini
// functionDeclarations: one group, deduplicated by name, names unchanged.
func functionDeclarations(raw json.RawMessage, toolChoice json.RawMessage) []any {
	if toolChoiceIsNone(toolChoice) || len(raw) == 0 {
		return nil
	}
	var tools []struct {
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var declarations []any
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		declaration := map[string]any{"name": name}
		if description := strings.TrimSpace(tool.Function.Description); description != "" {
			declaration["description"] = description
		}
		declaration["parameters"] = cleanSchema(tool.Function.Parameters)
		declarations = append(declarations, declaration)
	}
	return declarations
}

// toolChoiceIsNone reports whether the caller forbade tool use.
func toolChoiceIsNone(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.EqualFold(strings.TrimSpace(text), "none")
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err == nil {
		if kind, ok := object["type"].(string); ok {
			return strings.EqualFold(strings.TrimSpace(kind), "none")
		}
	}
	return false
}

// unsupportedSchemaKeys are JSON Schema keywords Gemini rejects outright.
var unsupportedSchemaKeys = map[string]bool{
	"$schema":               true,
	"additionalProperties":  true,
	"$defs":                 true,
	"definitions":           true,
	"unevaluatedProperties": true,
	"exclusiveMinimum":      true,
	"exclusiveMaximum":      true,
	"const":                 true,
	"examples":              true,
	"$ref":                  true,
}

// emptyObjectSchema is the placeholder Gemini needs when a declared object has
// no properties.
func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"reason": map[string]any{"type": "string"}},
	}
}

// cleanSchema normalizes a JSON Schema for Gemini: lowercase type names, drop
// keys the upstream rejects, and give an empty object a placeholder property.
func cleanSchema(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return emptyObjectSchema()
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return emptyObjectSchema()
	}
	cleaned, ok := cleanSchemaValue(schema).(map[string]any)
	if !ok {
		return emptyObjectSchema()
	}
	return cleaned
}

// cleanSchemaValue recursively normalizes a schema node.
func cleanSchemaValue(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		if list, ok := value.([]any); ok {
			out := make([]any, 0, len(list))
			for _, item := range list {
				out = append(out, cleanSchemaValue(item))
			}
			return out
		}
		return value
	}
	out := map[string]any{}
	for key, raw := range object {
		if unsupportedSchemaKeys[key] {
			continue
		}
		switch key {
		case "type":
			if text, ok := raw.(string); ok {
				out[key] = strings.ToLower(text)
				continue
			}
			out[key] = raw
		case "properties", "items", "anyOf", "oneOf", "allOf":
			out[key] = cleanSchemaValue(raw)
		default:
			out[key] = raw
		}
	}
	// Gemini needs at least one property when the schema declares an object.
	if kind, _ := out["type"].(string); kind == "object" {
		properties, ok := out["properties"].(map[string]any)
		if !ok || len(properties) == 0 {
			out["properties"] = map[string]any{"reason": map[string]any{"type": "string"}}
		}
	}
	return out
}
