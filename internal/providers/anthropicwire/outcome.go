package anthropicwire

import (
	"encoding/json"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// OutcomeFromAggregate turns the OpenAI-shaped map that Aggregate returns into
// the provider-neutral non-stream result. It lives here because every Anthropic
// Messages adapter needs the identical conversion, and a second copy is how the
// usage fields on one provider quietly start reading zero.
func OutcomeFromAggregate(aggregate map[string]any) (providers.ChatOutcome, error) {
	raw, err := json.Marshal(aggregate)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	var parsed struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int  `json:"prompt_tokens"`
			CompletionTokens int  `json:"completion_tokens"`
			CacheReadTokens  *int `json:"cache_read_tokens"`
			CacheWriteTokens *int `json:"cache_write_tokens"`
		} `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return providers.ChatOutcome{}, err
	}
	out := providers.ChatOutcome{UsageSource: "upstream", FinishReason: "stop"}
	if len(parsed.Choices) > 0 {
		out.Content = parsed.Choices[0].Message.Content
		out.Reasoning = parsed.Choices[0].Message.ReasoningContent
		out.ToolCalls = parsed.Choices[0].Message.ToolCalls
		if parsed.Choices[0].FinishReason != "" {
			out.FinishReason = parsed.Choices[0].FinishReason
		}
	}
	out.Model = parsed.Model
	out.PromptTokens = parsed.Usage.PromptTokens
	out.CompletionTokens = parsed.Usage.CompletionTokens
	out.CacheReadTokens = parsed.Usage.CacheReadTokens
	out.CacheWriteTokens = parsed.Usage.CacheWriteTokens
	return out, nil
}
