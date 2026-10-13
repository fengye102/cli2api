package antigravity

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// v1internal SSE -> chat SSE state machine.
//
// Each upstream frame is a v1internal envelope: the real Gemini response lives
// under the "response" key, falling back to the top level. Parts fold as
// follows:
//
//	.text + thought != true   -> delta.content
//	.text + thought == true   -> delta.reasoning_content
//	.functionCall{name,args}  -> one complete tool_calls delta (tool calls are
//	                             whole events upstream, not announced then
//	                             streamed in pieces)
//
// A frame carrying only a thoughtSignature is held back and attached to the
// functionCall that follows it.

// geminiStream is the per-stream translation state.
type geminiStream struct {
	model        string
	id           string
	created      int64
	started      bool
	finished     bool
	sawFinish    bool
	finishReason string
	toolIndex    int
	toolsSent    bool
	usage        map[string]any
	thoughtSig   string
}

func newGeminiStream(model string) *geminiStream {
	return &geminiStream{model: model, created: time.Now().Unix()}
}

// consume folds one upstream event into chat frames. The second return reports
// whether the stream ended.
func (s *geminiStream) consume(event map[string]any) ([]string, bool) {
	if s.finished {
		return nil, true
	}
	inner := event
	if nested, ok := event["response"].(map[string]any); ok {
		inner = nested
	}
	if errorObject, ok := inner["error"].(map[string]any); ok && len(errorObject) > 0 {
		frames, _ := s.fail(streamErrorMessage(errorObject))
		return frames, true
	}
	if s.id == "" {
		if id, _ := inner["responseId"].(string); strings.TrimSpace(id) != "" {
			s.id = strings.TrimSpace(id)
		}
	}
	if usage, ok := inner["usageMetadata"].(map[string]any); ok {
		s.usage = usageMetadata(usage)
	}
	candidates, _ := inner["candidates"].([]any)
	if len(candidates) == 0 {
		return nil, false
	}
	candidate, _ := candidates[0].(map[string]any)
	var frames []string
	if content, ok := candidate["content"].(map[string]any); ok {
		if parts, ok := content["parts"].([]any); ok {
			for _, part := range parts {
				frames = append(frames, s.consumePart(part)...)
			}
		}
	}
	if reason, _ := candidate["finishReason"].(string); strings.TrimSpace(reason) != "" {
		s.sawFinish = true
		s.finishReason = mapFinishReason(reason)
	}
	return frames, false
}

// consumePart folds one content part into frames.
func (s *geminiStream) consumePart(part any) []string {
	object, ok := part.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"thoughtSignature", "thought_signature"} {
		if signature, _ := object[key].(string); strings.TrimSpace(signature) != "" {
			s.thoughtSig = strings.TrimSpace(signature)
		}
	}
	if call, ok := object["functionCall"].(map[string]any); ok && len(call) > 0 {
		signature := s.thoughtSig
		s.thoughtSig = ""
		return s.toolCallFrames(call, signature)
	}
	text, _ := object["text"].(string)
	if text == "" {
		return nil
	}
	isThought, _ := object["thought"].(bool)
	frames := s.start()
	if isThought {
		frames = append(frames, s.deltaFrame(map[string]any{"reasoning_content": text}))
	} else {
		frames = append(frames, s.deltaFrame(map[string]any{"content": text}))
	}
	return frames
}

// toolCallFrames emits one complete tool_calls delta.
func (s *geminiStream) toolCallFrames(call map[string]any, signature string) []string {
	name, _ := call["name"].(string)
	id, _ := call["id"].(string)
	if strings.TrimSpace(id) == "" {
		id = "call_" + hexString(randomBytes(8))
	}
	arguments := "{}"
	switch args := call["args"].(type) {
	case string:
		arguments = args
	case nil:
		arguments = "{}"
	default:
		if encoded, err := json.Marshal(args); err == nil {
			arguments = string(encoded)
		}
	}
	tool := map[string]any{
		"index":    s.toolIndex,
		"id":       id,
		"type":     "function",
		"function": map[string]any{"name": name, "arguments": arguments},
	}
	s.toolIndex++
	s.toolsSent = true
	if signature != "" {
		// Google's own OpenAI-compatible endpoint carries the signature here; the
		// request side reads the same location back.
		tool["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": signature}}
	}
	frames := s.start()
	return append(frames, s.deltaFrame(map[string]any{"tool_calls": []any{tool}}))
}

// start emits the opening assistant frame exactly once.
func (s *geminiStream) start() []string {
	if s.started {
		return nil
	}
	s.started = true
	if s.id == "" {
		s.id = "chatcmpl_" + hexString(randomBytes(12))
	}
	return []string{s.deltaFrame(map[string]any{"role": "assistant", "content": ""})}
}

// complete finishes the stream: finish frame, usage frame, then [DONE].
func (s *geminiStream) complete() []string {
	if s.finished {
		return nil
	}
	if !s.started && s.finishReason == "" {
		frames, _ := s.fail("上游返回空响应（没有任何内容帧）")
		return frames
	}
	s.finished = true
	reason := s.finishReason
	if reason == "" {
		reason = "stop"
	}
	// A tool call that ended with stop must report tool_calls, or the client
	// treats the turn as finished conversation.
	if reason == "stop" && s.toolsSent {
		reason = "tool_calls"
	}
	frames := s.start()
	frames = append(frames, s.finishFrame(reason))
	frames = append(frames, s.chatFrame(map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{},
		"usage":   s.usagePayload(),
	}))
	frames = append(frames, "data: [DONE]\n\n")
	return frames
}

// fail emits an error frame plus [DONE] exactly once.
func (s *geminiStream) fail(message string) ([]string, bool) {
	if s.finished {
		return nil, true
	}
	s.finished = true
	errorObject := map[string]any{"message": message, "type": "upstream_error", "status": 502}
	frame := map[string]any{"error": errorObject}
	return []string{s.chatFrame(frame), "data: [DONE]\n\n"}, true
}

// usagePayload renders usage in the chat shape (zeros when absent).
func (s *geminiStream) usagePayload() map[string]any {
	if s.usage != nil {
		return s.usage
	}
	return map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
}

// deltaFrame is one delta frame.
func (s *geminiStream) deltaFrame(delta map[string]any) string {
	return s.chatFrame(map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	})
}

// finishFrame is the closing frame (empty delta, finish_reason).
func (s *geminiStream) finishFrame(reason string) string {
	return s.chatFrame(map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}},
	})
}

// chatFrame serializes one frame as an SSE data line.
func (s *geminiStream) chatFrame(payload map[string]any) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "data: {\"error\":{\"message\":\"frame encoding failed\"}}\n\n"
	}
	return "data: " + string(encoded) + "\n\n"
}

// mapFinishReason normalizes an upstream finishReason.
func mapFinishReason(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	default:
		return "stop"
	}
}

// usageMetadata renders Gemini usage in the chat shape. Completion tokens
// include thoughts, and cached content maps to prompt_tokens_details.
func usageMetadata(usage map[string]any) map[string]any {
	number := func(key string) int64 {
		switch typed := usage[key].(type) {
		case float64:
			return int64(typed)
		case json.Number:
			if value, err := typed.Int64(); err == nil {
				return value
			}
		}
		return 0
	}
	prompt := number("promptTokenCount")
	thoughts := number("thoughtsTokenCount")
	candidates := number("candidatesTokenCount")
	total := number("totalTokenCount")
	if candidates == 0 && total > 0 {
		candidates = total - prompt - thoughts
		if candidates < 0 {
			candidates = 0
		}
	}
	if total == 0 {
		total = prompt + candidates + thoughts
	}
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": candidates + thoughts,
		"total_tokens":      total,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": number("cachedContentTokenCount"),
		},
	}
}

// streamErrorMessage reads an upstream error object's message.
func streamErrorMessage(errorObject map[string]any) string {
	if message, _ := errorObject["message"].(string); strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	if code, ok := errorObject["code"]; ok {
		return fmt.Sprintf("上游流式返回错误（code %v）", code)
	}
	return "上游流式返回错误"
}

// randomBytes returns n random bytes; a failure of the random source yields
// zero bytes, which is harmless for an id that only needs to look unique.
func randomBytes(n int) []byte {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return raw
}

// translateGeminiStream converts a v1internal SSE body into the chat SSE stream
// the gateway relays. Writes go straight to dst so reader back-pressure applies
// upstream.
//
// The stream ends on the first finishReason, on `data: [DONE]`, or on EOF. An
// EOF with neither means the upstream truncated the response, which is reported
// as a stream error rather than a silent success.
func translateGeminiStream(src io.Reader, dst io.Writer, model string) error {
	state := newGeminiStream(model)
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	sawFinish := false
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			sawFinish = true
			break
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		frames, ended := state.consume(event)
		if err := writeFrames(dst, frames); err != nil {
			return err
		}
		if ended {
			sawFinish = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !sawFinish {
		frames, _ := state.fail("上游流在收到结束原因之前结束（响应被截断）")
		return writeFrames(dst, frames)
	}
	return writeFrames(dst, state.complete())
}

// writeFrames writes pre-rendered SSE frames, stopping at the first failure.
func writeFrames(dst io.Writer, frames []string) error {
	for _, frame := range frames {
		if _, err := io.WriteString(dst, frame); err != nil {
			return err
		}
	}
	return nil
}
