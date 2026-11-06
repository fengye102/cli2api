package runable

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Runable's chat endpoint answers with an AI SDK data stream, not OpenAI SSE:
// every frame is `data: {"type":"…"}` and the text arrives in `text-delta`
// frames whose payload field changed name between SDK majors (delta, textDelta,
// or a bare text). Usage comes either from the closing `finish` frame or from the
// Anthropic metadata the provider attaches. This file turns that into the
// chat.completion.chunk dialect the gateway relays, and assembles the same stream
// for non-stream callers.

// usage carries the token counts a turn reported.
type usage struct {
	Input  int
	Output int
}

func (u usage) total() int { return u.Input + u.Output }

// scanFrames walks the data frames of an AI SDK stream. A frame that is not JSON
// is not silently dropped: the reference implementation found the upstream
// occasionally emitting a bare string frame, so that text is reported as a delta
// through onLoose.
func scanFrames(reader io.Reader, handle func(map[string]any) error, onLoose func(string)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			if onLoose != nil {
				if text := looseText(data); text != "" {
					onLoose(text)
				}
			}
			continue
		}
		if err := handle(frame); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// looseText recovers text from a frame that is not an object: either a quoted
// string or the SDK's `0:"…"` prefix form.
func looseText(data string) string {
	if idx := strings.Index(data, ":"); idx > 0 {
		head := strings.TrimSpace(data[:idx])
		allDigits := head != ""
		for _, r := range head {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			rest := strings.TrimSpace(data[idx+1:])
			rest = strings.Trim(rest, "\"")
			rest = strings.ReplaceAll(rest, `\"`, `"`)
			return rest
		}
	}
	if strings.HasPrefix(data, "\"") && strings.HasSuffix(data, "\"") {
		return strings.Trim(data, "\"")
	}
	return ""
}

// deltaText reads the text out of a frame, whichever spelling the SDK used.
func deltaText(frame map[string]any) string {
	kind, _ := frame["type"].(string)
	switch kind {
	case "text-delta", "text", "":
		for _, key := range []string{"delta", "textDelta", "text", "content"} {
			if value, ok := frame[key].(string); ok && value != "" {
				return value
			}
		}
	}
	return ""
}

// reasoningText reads the text out of a reasoning frame.
func reasoningText(frame map[string]any) string {
	kind, _ := frame["type"].(string)
	if kind != "reasoning-delta" && kind != "reasoning" {
		return ""
	}
	for _, key := range []string{"delta", "textDelta", "text"} {
		if value, ok := frame[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// streamError turns an error frame into an error the pipe can carry.
func streamError(frame map[string]any) error {
	kind, _ := frame["type"].(string)
	if kind != "error" {
		return nil
	}
	message := firstNonEmpty(str(frame["errorText"]), str(frame["message"]), str(frame["error"]))
	if message == "" {
		message = "runable 流内错误"
	}
	return &streamFailure{Message: message}
}

// streamFailure is an upstream error that arrived inside a 200 stream.
type streamFailure struct {
	Message string
}

func (e *streamFailure) Error() string { return e.Message }

func readUsage(frame map[string]any, target *usage) {
	if target == nil {
		return
	}
	// The closing finish frame carries totalUsage; older builds used usage.
	for _, key := range []string{"totalUsage", "usage"} {
		if nested, ok := frame[key].(map[string]any); ok {
			if value := intField(nested, "inputTokens", "promptTokens"); value > 0 {
				target.Input = value
			}
			if value := intField(nested, "outputTokens", "completionTokens"); value > 0 {
				target.Output = value
			}
			if value := intField(nested, "totalTokens"); value > 0 && target.Output == 0 {
				target.Output = value
			}
		}
	}
	// Anthropic metadata rides along on the custom frames.
	if kind, _ := frame["type"].(string); kind == "custom" {
		if metadata, ok := frame["providerMetadata"].(map[string]any); ok {
			if anthropic, ok := metadata["anthropic"].(map[string]any); ok {
				if nested, ok := anthropic["usage"].(map[string]any); ok {
					if value := intField(nested, "input_tokens", "inputTokens"); value > 0 {
						target.Input = value
					}
					if value := intField(nested, "output_tokens", "outputTokens"); value > 0 {
						target.Output = value
					}
				}
			}
		}
	}
	// The top-level usage frame the SDK emits before finish.
	if nested, ok := frame["usage"].(map[string]any); ok {
		if value := intField(nested, "inputTokens", "promptTokens"); value > 0 {
			target.Input = value
		}
		if value := intField(nested, "outputTokens", "completionTokens"); value > 0 {
			target.Output = value
		}
	}
}

func intField(object map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, ok := object[key].(float64); ok {
			return int(value)
		}
	}
	return 0
}

func str(value any) string {
	text, _ := value.(string)
	return text
}

// finishReason maps the SDK's reason onto the OpenAI vocabulary.
func finishReason(frame map[string]any) string {
	reason := firstNonEmpty(str(frame["finishReason"]), str(frame["reason"]))
	reason = strings.ToLower(reason)
	switch reason {
	case "length", "max_tokens", "max-tokens":
		return "length"
	case "tool-calls", "tool_calls":
		return "tool_calls"
	case "content-filter", "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

// streamState is the OpenAI chunk sequence being built for one turn.
type streamState struct {
	id      string
	model   string
	created int64
	roleSet bool
	finish  string
}

func newStreamState(model string) *streamState {
	return &streamState{
		id:      fmt.Sprintf("chatcmpl-rnbl-%d", time.Now().UnixNano()),
		model:   model,
		created: time.Now().Unix(),
	}
}

func (s *streamState) chunk(delta map[string]any, finish string, usage *usage) ([]byte, error) {
	if !s.roleSet {
		s.roleSet = true
		delta["role"] = "assistant"
	}
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	payload := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]any{choice},
	}
	if usage != nil {
		payload["usage"] = map[string]any{
			"prompt_tokens":     usage.Input,
			"completion_tokens": usage.Output,
			"total_tokens":      usage.total(),
		}
	}
	return json.Marshal(payload)
}

// rewriteStream converts the upstream AI SDK stream into OpenAI chunks. Headers
// stay 200: an upstream failure mid-stream is surfaced as a pipe error, which the
// API layer turns into a truncated stream plus a logged failure.
func rewriteStream(resp *http.Response, model string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		defer pw.Close()
		state := newStreamState(model)
		var counts usage
		write := func(payload []byte) error {
			_, err := fmt.Fprintf(pw, "data: %s\n\n", payload)
			return err
		}
		err := scanFrames(resp.Body, func(frame map[string]any) error {
			if err := streamError(frame); err != nil {
				return err
			}
			readUsage(frame, &counts)
			if text := deltaText(frame); text != "" {
				payload, err := state.chunk(map[string]any{"content": text}, "", nil)
				if err != nil {
					return err
				}
				return write(payload)
			}
			if text := reasoningText(frame); text != "" {
				payload, err := state.chunk(map[string]any{"reasoning_content": text}, "", nil)
				if err != nil {
					return err
				}
				return write(payload)
			}
			if kind, _ := frame["type"].(string); kind == "finish" {
				state.finish = finishReason(frame)
			}
			return nil
		}, func(text string) {
			if payload, err := state.chunk(map[string]any{"content": text}, "", nil); err == nil {
				_ = write(payload)
			}
		})
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		finish := state.finish
		if finish == "" {
			finish = "stop"
		}
		if payload, err := state.chunk(map[string]any{}, finish, nil); err == nil {
			_ = write(payload)
		}
		if counts.total() > 0 {
			if payload, err := state.chunk(map[string]any{}, "", &counts); err == nil {
				_ = write(payload)
			}
		}
		_, _ = io.WriteString(pw, "data: [DONE]\n\n")
	}()
	out := *resp
	out.Body = pr
	out.ContentLength = -1
	header := resp.Header.Clone()
	header.Del("Content-Length")
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	out.Header = header
	return &out
}

// collectStream assembles one stream into a single answer.
func collectStream(reader io.Reader) (content, reasoning string, counts usage, finish string, err error) {
	var text, thoughts strings.Builder
	err = scanFrames(reader, func(frame map[string]any) error {
		if err := streamError(frame); err != nil {
			return err
		}
		readUsage(frame, &counts)
		if chunk := deltaText(frame); chunk != "" {
			text.WriteString(chunk)
		}
		if chunk := reasoningText(frame); chunk != "" {
			thoughts.WriteString(chunk)
		}
		if kind, _ := frame["type"].(string); kind == "finish" {
			finish = finishReason(frame)
		}
		return nil
	}, func(loose string) {
		text.WriteString(loose)
	})
	if finish == "" {
		finish = "stop"
	}
	return text.String(), thoughts.String(), counts, finish, err
}
