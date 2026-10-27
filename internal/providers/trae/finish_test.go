package trae

import (
	"strings"
	"testing"
)

// TestReconcileTraeFinish pins the rule that a turn carrying tool calls reports
// finish_reason "tool_calls" regardless of what upstream claimed. Solo closes
// tool-only turns with an empty finish_reason; the old fallback reported "stop",
// which OpenAI clients read as "the assistant finished talking" — so a reply
// that was nothing but tool calls looked empty, the agent retried, and the turn
// died with "no output".
func TestReconcileTraeFinish(t *testing.T) {
	for _, test := range []struct {
		name         string
		finish       string
		sawToolCalls bool
		want         string
	}{
		{name: "empty finish with tool calls", finish: "", sawToolCalls: true, want: "tool_calls"},
		{name: "stop with tool calls", finish: "stop", sawToolCalls: true, want: "tool_calls"},
		{name: "length with tool calls", finish: "length", sawToolCalls: true, want: "tool_calls"},
		{name: "empty finish no tool calls", finish: "", sawToolCalls: false, want: "stop"},
		{name: "stop stays stop", finish: "stop", sawToolCalls: false, want: "stop"},
		{name: "length preserved", finish: "length", sawToolCalls: false, want: "length"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := reconcileTraeFinish(test.finish, test.sawToolCalls); got != test.want {
				t.Fatalf("reconcileTraeFinish(%q, %v) = %q, want %q",
					test.finish, test.sawToolCalls, got, test.want)
			}
		})
	}
}

// buildSoloSSE renders Solo's event framing: "event: <name>\ndata: <json>\n\n".
func buildSoloSSE(events ...[2]string) string {
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

func TestAggregateSoloReportsToolCalls(t *testing.T) {
	// A tool-only turn: upstream sends the tool_calls in an output event and a
	// done event with no finish_reason. The aggregate must not claim "stop".
	body := buildSoloSSE(
		[2]string{"metadata", `{"model":"GLM-5.3-Flash"}`},
		[2]string{"output", `{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/etc/hosts\"}"}}]}`},
		[2]string{"token_usage", `{"prompt_tokens":20,"completion_tokens":8}`},
		[2]string{"done", `{}`},
	)
	out, err := Aggregate(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, ok := out["choices"].([]map[string]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices = %#v", out["choices"])
	}
	if finish, _ := choices[0]["finish_reason"].(string); finish != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finish)
	}
	msg, _ := choices[0]["message"].(map[string]any)
	if calls, _ := msg["tool_calls"].([]any); len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
}

func TestAggregateSoloKeepsStopForTextReplies(t *testing.T) {
	body := buildSoloSSE(
		[2]string{"metadata", `{"model":"GLM-5.3-Flash"}`},
		[2]string{"output", `{"response":"收到"}`},
		[2]string{"done", `{"finish_reason":"stop"}`},
	)
	out, err := Aggregate(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, _ := out["choices"].([]map[string]any)
	if len(choices) == 0 {
		t.Fatal("no choices")
	}
	if finish, _ := choices[0]["finish_reason"].(string); finish != "stop" {
		t.Fatalf("finish_reason = %q, want stop", finish)
	}
}
