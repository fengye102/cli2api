package zcode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Anthropic Messages SSE event types this translator understands. Anything
// else (e.g. ping) is ignored; an `error` event surfaces as a stream error.
type anthropicEvent struct {
	Name string
	Data json.RawMessage
}

// scanAnthropic splits an Anthropic SSE body into typed events. It is the
// minimal framing helper shared by the streaming rewriter and the non-stream
// aggregator.
func scanAnthropic(reader io.Reader, handle func(anthropicEvent) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var event string
	var data bytes.Buffer
	flush := func() error {
		if event == "" && data.Len() == 0 {
			return nil
		}
		ev := anthropicEvent{Name: event, Data: append(json.RawMessage(nil), data.Bytes()...)}
		event = ""
		data.Reset()
		if ev.Name == "" {
			ev.Name = "message"
		}
		return handle(ev)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(payload)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// streamState tracks the OpenAI chunk stream being built: per-content-block
// tool_call identity, text vs reasoning blocks, and the last finish_reason.
type streamState struct {
	id           string
	model        string
	created      int64
	roleSent     bool
	blockTool    map[int]bool   // content block index -> is tool_use
	blockIndex   map[int]int    // content block index -> tool_calls index
	toolNames    map[int]string // tool_calls index -> name
	toolIDs      map[int]string // tool_calls index -> id
	finishReason string
	usage        map[string]any
	stopSeen     bool
}

func newStreamState() *streamState {
	return &streamState{
		created:    time.Now().Unix(),
		blockTool:  map[int]bool{},
		blockIndex: map[int]int{},
		toolNames:  map[int]string{},
		toolIDs:    map[int]string{},
	}
}

// chunkFor emits one chat.completion.chunk frame for the given delta fields
// and optional finish_reason. Empty deltas with no finish are dropped by the
// caller before this is invoked.
func (s *streamState) chunkFor(delta map[string]any, finish string, usage map[string]any) ([]byte, error) {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	} else {
		choice["finish_reason"] = nil
	}
	chunk := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]any{choice},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return json.Marshal(chunk)
}

func (s *streamState) roleDelta() map[string]any {
	if s.roleSent {
		return map[string]any{}
	}
	s.roleSent = true
	return map[string]any{"role": "assistant"}
}

// rewriteChatStream converts an upstream Anthropic Messages SSE body into an
// OpenAI chat.completion.chunk stream the executor can relay. Headers stay
// 200 OK; protocol errors arriving mid-stream are surfaced as a trailing
// error event the executor reads once the pipe drains.
func rewriteChatStream(resp *http.Response) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		defer pw.Close()
		if err := translateAnthropicStream(resp.Body, pw); err != nil {
			_ = pw.CloseWithError(err)
		}
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

// translateAnthropicStream drives the per-event state machine. Writes go
// directly to dst so back-pressure from the reader applies upstream.
func translateAnthropicStream(src io.Reader, dst io.Writer) error {
	state := newStreamState()
	write := func(payload []byte) error {
		_, err := fmt.Fprintf(dst, "data: %s\n\n", payload)
		return err
	}
	emit := func(delta map[string]any, finish string, usage map[string]any) error {
		if !state.roleSent {
			for k, v := range state.roleDelta() {
				delta[k] = v
			}
		}
		if len(delta) == 0 && finish == "" && usage == nil {
			return nil
		}
		chunk, err := state.chunkFor(delta, finish, usage)
		if err != nil {
			return err
		}
		return write(chunk)
	}

	err := scanAnthropic(src, func(ev anthropicEvent) error {
		switch ev.Name {
		case "message_start":
			var msg struct {
				Message struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Usage struct {
						InputTokens      int `json:"input_tokens"`
						CacheReadTokens  int `json:"cache_read_input_tokens"`
						CacheWriteTokens int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(ev.Data, &msg); err != nil {
				return nil
			}
			if msg.Message.ID != "" {
				state.id = msg.Message.ID
			}
			if state.id == "" {
				state.id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
			}
			if msg.Message.Model != "" {
				state.model = msg.Message.Model
			}
			if msg.Message.Usage.InputTokens > 0 {
				state.usage = map[string]any{"prompt_tokens": msg.Message.Usage.InputTokens}
				if msg.Message.Usage.CacheReadTokens > 0 {
					state.usage["cache_read_tokens"] = msg.Message.Usage.CacheReadTokens
				}
				if msg.Message.Usage.CacheWriteTokens > 0 {
					state.usage["cache_write_tokens"] = msg.Message.Usage.CacheWriteTokens
				}
			}
			return nil
		case "content_block_start":
			var evt struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(ev.Data, &evt); err != nil {
				return nil
			}
			if evt.ContentBlock.Type == "tool_use" {
				toolIdx := len(state.blockIndex)
				state.blockTool[evt.Index] = true
				state.blockIndex[evt.Index] = toolIdx
				state.toolIDs[toolIdx] = evt.ContentBlock.ID
				state.toolNames[toolIdx] = evt.ContentBlock.Name
				return emit(map[string]any{
					"tool_calls": []any{map[string]any{
						"index": toolIdx,
						"id":    evt.ContentBlock.ID,
						"type":  "function",
						"function": map[string]any{
							"name":      evt.ContentBlock.Name,
							"arguments": "",
						},
					}},
				}, "", nil)
			}
			return nil
		case "content_block_delta":
			var evt struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(ev.Data, &evt); err != nil {
				return nil
			}
			switch evt.Delta.Type {
			case "text_delta":
				if evt.Delta.Text == "" {
					return nil
				}
				return emit(map[string]any{"content": evt.Delta.Text}, "", nil)
			case "thinking_delta":
				if evt.Delta.Thinking == "" {
					return nil
				}
				// Reasoning rides in reasoning_content like the other
				// providers that already emit a separate thinking channel.
				return emit(map[string]any{"reasoning_content": evt.Delta.Thinking}, "", nil)
			case "input_json_delta":
				toolIdx, ok := state.blockIndex[evt.Index]
				if !ok || evt.Delta.PartialJSON == "" {
					return nil
				}
				return emit(map[string]any{
					"tool_calls": []any{map[string]any{
						"index": toolIdx,
						"function": map[string]any{
							"arguments": evt.Delta.PartialJSON,
						},
					}},
				}, "", nil)
			}
			return nil
		case "message_delta":
			var evt struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(ev.Data, &evt); err != nil {
				return nil
			}
			if evt.Delta.StopReason != "" {
				state.finishReason = anthropicStopToOpenAI(evt.Delta.StopReason)
			}
			if evt.Usage.OutputTokens > 0 {
				if state.usage == nil {
					state.usage = map[string]any{}
				}
				state.usage["completion_tokens"] = evt.Usage.OutputTokens
			}
			return nil
		case "message_stop":
			state.stopSeen = true
			finish := state.finishReason
			if finish == "" {
				finish = "stop"
			}
			usage := state.usage
			if usage != nil {
				if p, ok := usage["prompt_tokens"].(int); ok {
					if c, ok := usage["completion_tokens"].(int); ok {
						usage["total_tokens"] = p + c
					}
				}
			}
			if err := emit(map[string]any{}, finish, usage); err != nil {
				return err
			}
			_, err := io.WriteString(dst, "data: [DONE]\n\n")
			return err
		case "error":
			// Anthropic emits error payloads with {type:"error",error:{type,message}}.
			// Surface as a stream error; the caller classifies.
			var evt struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(ev.Data, &evt); err != nil {
				return fmt.Errorf("zcode stream error: %s", string(ev.Data))
			}
			return fmt.Errorf("zcode stream error type=%s: %s", evt.Error.Type, evt.Error.Message)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !state.stopSeen {
		// Upstream closed early: emit a terminating chunk so the client does
		// not hang waiting for a finish frame.
		if state.id == "" {
			state.id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
		}
		finish := state.finishReason
		if finish == "" {
			finish = "stop"
		}
		if err := emit(map[string]any{}, finish, state.usage); err != nil {
			return err
		}
		_, err := io.WriteString(dst, "data: [DONE]\n\n")
		return err
	}
	return nil
}

func anthropicStopToOpenAI(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	return "stop"
}

// Aggregate consumes a full Anthropic SSE body and returns a single OpenAI
// chat.completion object. Used by ChatNonStream; mirrors the workbuddy
// Aggregate shape.
func Aggregate(reader io.Reader) (map[string]any, error) {
	result := map[string]any{"object": "chat.completion"}
	message := map[string]any{"role": "assistant"}
	toolCalls := map[int]map[string]any{}
	finishReason := "stop"
	var content, reasoning strings.Builder
	var order []int
	sawStop := false
	var usage map[string]any
	model := ""
	id := ""

	state := newStreamState()
	err := scanAnthropic(reader, func(ev anthropicEvent) error {
		switch ev.Name {
		case "message_start":
			var msg struct {
				Message struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Usage struct {
						InputTokens      int `json:"input_tokens"`
						CacheReadTokens  int `json:"cache_read_input_tokens"`
						CacheWriteTokens int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(ev.Data, &msg); err != nil {
				return nil
			}
			id = msg.Message.ID
			model = msg.Message.Model
			if msg.Message.Usage.InputTokens > 0 {
				usage = map[string]any{"prompt_tokens": msg.Message.Usage.InputTokens}
				if msg.Message.Usage.CacheReadTokens > 0 {
					usage["cache_read_tokens"] = msg.Message.Usage.CacheReadTokens
				}
				if msg.Message.Usage.CacheWriteTokens > 0 {
					usage["cache_write_tokens"] = msg.Message.Usage.CacheWriteTokens
				}
			}
		case "content_block_start":
			var evt struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(ev.Data, &evt); err != nil {
				return nil
			}
			if evt.ContentBlock.Type == "tool_use" {
				toolIdx := len(state.blockIndex)
				state.blockTool[evt.Index] = true
				state.blockIndex[evt.Index] = toolIdx
				state.toolIDs[toolIdx] = evt.ContentBlock.ID
				state.toolNames[toolIdx] = evt.ContentBlock.Name
			}
		case "content_block_delta":
			var evt struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(ev.Data, &evt); err != nil {
				return nil
			}
			switch evt.Delta.Type {
			case "text_delta":
				content.WriteString(evt.Delta.Text)
			case "thinking_delta":
				reasoning.WriteString(evt.Delta.Thinking)
			case "input_json_delta":
				toolIdx, ok := state.blockIndex[evt.Index]
				if !ok {
					return nil
				}
				call, exists := toolCalls[toolIdx]
				if !exists {
					call = map[string]any{
						"index": toolIdx,
						"id":    state.toolIDs[toolIdx],
						"type":  "function",
						"function": map[string]any{
							"name":      state.toolNames[toolIdx],
							"arguments": "",
						},
					}
					toolCalls[toolIdx] = call
					order = append(order, toolIdx)
				}
				fn, _ := call["function"].(map[string]any)
				prev, _ := fn["arguments"].(string)
				fn["arguments"] = prev + evt.Delta.PartialJSON
			}
		case "message_delta":
			var evt struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(ev.Data, &evt); err != nil {
				return nil
			}
			if evt.Delta.StopReason != "" {
				finishReason = anthropicStopToOpenAI(evt.Delta.StopReason)
			}
			if evt.Usage.OutputTokens > 0 {
				if usage == nil {
					usage = map[string]any{}
				}
				usage["completion_tokens"] = evt.Usage.OutputTokens
			}
		case "message_stop":
			sawStop = true
		case "error":
			var evt struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(ev.Data, &evt); err != nil {
				return fmt.Errorf("zcode stream error: %s", string(ev.Data))
			}
			return fmt.Errorf("zcode stream error type=%s: %s", evt.Error.Type, evt.Error.Message)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !sawStop {
		return nil, fmt.Errorf("zcode stream ended before message_stop")
	}

	message["content"] = content.String()
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		sort.Ints(order)
		calls := make([]any, 0, len(order))
		for _, index := range order {
			calls = append(calls, toolCalls[index])
		}
		message["tool_calls"] = calls
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	result["id"] = id
	if model != "" {
		result["model"] = model
	}
	if usage != nil {
		if p, ok := usage["prompt_tokens"].(int); ok {
			if c, ok := usage["completion_tokens"].(int); ok {
				usage["total_tokens"] = p + c
			}
		}
		result["usage"] = usage
	}
	result["created"] = time.Now().Unix()
	result["choices"] = []map[string]any{{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}}
	return result, nil
}
