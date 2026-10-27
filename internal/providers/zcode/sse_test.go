package zcode

import (
	"strings"
	"testing"
)

// anthropicSSE builds an Anthropic Messages SSE body the way the ZCode plan
// gateway emits it. Each event is "event: <name>\ndata: <json>\n\n".
func anthropicSSE(events ...[2]string) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("event: ")
		b.WriteString(ev[0])
		b.WriteString("\ndata: ")
		b.WriteString(ev[1])
		b.WriteString("\n\n")
	}
	return b.String()
}

// aggregateFinish runs Aggregate and digs the finish_reason out of
// choices[0], which is where the OpenAI shape carries it.
func aggregateFinish(t *testing.T, body string) (string, map[string]any) {
	t.Helper()
	out, err := Aggregate(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, ok := out["choices"].([]map[string]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices = %#v", out["choices"])
	}
	finish, _ := choices[0]["finish_reason"].(string)
	msg, _ := choices[0]["message"].(map[string]any)
	return finish, msg
}

func TestAggregateMapsToolUseToToolCalls(t *testing.T) {
	// A turn whose only content is a tool call. Upstream closes it with
	// stop_reason "tool_use", which must surface as finish_reason "tool_calls".
	body := anthropicSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"glm-5.3-flash","usage":{"input_tokens":42}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"read_file"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/etc/hosts\"}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	finish, msg := aggregateFinish(t, body)
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finish)
	}
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
}

func TestAggregateReportsToolCallsWhenUpstreamSaysEndTurn(t *testing.T) {
	// GLM on the plan gateway closes tool-call turns with "end_turn" (and
	// sometimes with no stop_reason at all). A plain mapping would report
	// "stop", which OpenAI clients read as "the assistant finished talking" —
	// the bug where a tool-only reply looks empty, gets retried, and the turn
	// dies with "no output".
	for _, stopReason := range []string{"end_turn", "stop_sequence", ""} {
		t.Run("stop_reason="+stopReason, func(t *testing.T) {
			delta := `{"type":"message_delta","delta":{},"usage":{"output_tokens":9}}`
			if stopReason != "" {
				delta = `{"type":"message_delta","delta":{"stop_reason":"` + stopReason + `"},"usage":{"output_tokens":9}}`
			}
			body := anthropicSSE(
				[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_2","model":"glm-5.3-flash","usage":{"input_tokens":10}}}`},
				[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_9","name":"terminal"}}`},
				[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`},
				[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
				[2]string{"message_delta", delta},
				[2]string{"message_stop", `{"type":"message_stop"}`},
			)
			finish, msg := aggregateFinish(t, body)
			if finish != "tool_calls" {
				t.Fatalf("finish_reason = %q, want tool_calls (stop_reason was %q)", finish, stopReason)
			}
			if calls, _ := msg["tool_calls"].([]any); len(calls) != 1 {
				t.Fatalf("tool_calls = %d, want 1", len(calls))
			}
		})
	}
}

func TestAggregateKeepsStopForPlainTextReplies(t *testing.T) {
	// A text-only turn must still report "stop" so we do not regress the
	// ordinary completion path.
	body := anthropicSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_3","model":"glm-5.3-flash","usage":{"input_tokens":8}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"收到"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	finish, _ := aggregateFinish(t, body)
	if finish != "stop" {
		t.Fatalf("finish_reason = %q, want stop", finish)
	}
}

func TestAggregateMapsMaxTokensToLength(t *testing.T) {
	body := anthropicSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_4","model":"glm-5.3-flash","usage":{"input_tokens":8}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":1}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	finish, _ := aggregateFinish(t, body)
	if finish != "length" {
		t.Fatalf("finish_reason = %q, want length", finish)
	}
}

func TestReconcileFinish(t *testing.T) {
	for _, test := range []struct {
		finish     string
		sawToolUse bool
		want       string
	}{
		{finish: "stop", sawToolUse: true, want: "tool_calls"},
		{finish: "", sawToolUse: true, want: "tool_calls"},
		{finish: "length", sawToolUse: true, want: "tool_calls"},
		{finish: "stop", sawToolUse: false, want: "stop"},
		{finish: "", sawToolUse: false, want: "stop"},
		{finish: "length", sawToolUse: false, want: "length"},
	} {
		if got := reconcileFinish(test.finish, test.sawToolUse); got != test.want {
			t.Errorf("reconcileFinish(%q, %v) = %q, want %q", test.finish, test.sawToolUse, got, test.want)
		}
	}
}
