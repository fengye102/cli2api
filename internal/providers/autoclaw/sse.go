package autoclaw

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

type sseAggregate struct {
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*toolCall
	order     []int
	usage     chatUsage
	finish    string
	model     string
}

type toolCall struct {
	id   string
	name string
	args strings.Builder
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// collectChatSSE folds an SSE body into one non-stream outcome.
//
// AutoClaw's proxy stamps every frame as {role:"assistant", content:...} with
// no finish_reason key, and emits newline-only heartbeat frames that also
// carry role. A frame whose content is nothing but whitespace is therefore
// dropped even though it has a role - otherwise the client sees a wall of
// blank deltas.
func collectChatSSE(body io.Reader, model string) (providers.ChatOutcome, error) {
	agg := &sseAggregate{tools: map[int]*toolCall{}, finish: "stop", model: model}
	raw, err := io.ReadAll(io.LimitReader(body, 32<<20))
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage chatUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Model != "" {
			agg.model = chunk.Model
		}
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			agg.usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			agg.collectDelta(choice.Delta)
			if reason := strings.TrimSpace(choice.FinishReason); reason != "" {
				agg.finish = reason
			}
		}
	}
	return agg.outcome(), nil
}

func (agg *sseAggregate) collectDelta(delta struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
	ToolCalls        []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}) {
	if text := delta.Content; text != "" {
		// Drop whitespace-only heartbeat frames; role alone does not count as
		// meaningful payload in this upstream.
		if strings.TrimSpace(text) != "" {
			agg.content.WriteString(text)
		}
	}
	if reasoning := firstNonEmpty(delta.ReasoningContent, delta.Reasoning); reasoning != "" {
		agg.reasoning.WriteString(reasoning)
	}
	for _, call := range delta.ToolCalls {
		slot, ok := agg.tools[call.Index]
		if !ok {
			slot = &toolCall{}
			agg.tools[call.Index] = slot
			agg.order = append(agg.order, call.Index)
		}
		if call.ID != "" {
			slot.id = call.ID
		}
		if call.Function.Name != "" {
			slot.name = call.Function.Name
		}
		slot.args.WriteString(call.Function.Arguments)
	}
}

func (agg *sseAggregate) outcome() providers.ChatOutcome {
	out := providers.ChatOutcome{
		Model:            agg.model,
		Content:          agg.content.String(),
		Reasoning:        agg.reasoning.String(),
		FinishReason:     agg.finish,
		PromptTokens:     agg.usage.PromptTokens,
		CompletionTokens: agg.usage.CompletionTokens,
	}
	if len(agg.order) > 0 {
		calls := make([]map[string]any, 0, len(agg.order))
		for _, index := range agg.order {
			slot := agg.tools[index]
			calls = append(calls, map[string]any{
				"id":   slot.id,
				"type": "function",
				"function": map[string]any{
					"name":      slot.name,
					"arguments": slot.args.String(),
				},
			})
		}
		encoded, err := json.Marshal(calls)
		if err == nil {
			out.ToolCalls = encoded
		}
		// A tool call must not be reported as a finished text turn: the agent
		// reading it would think the model stopped with no content.
		if out.FinishReason == "" || out.FinishReason == "stop" {
			out.FinishReason = "tool_calls"
		}
	}
	return out
}
