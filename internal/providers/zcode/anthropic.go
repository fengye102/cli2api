package zcode

import (
	"io"
	"net/http"

	"github.com/caigee-cmd/cli2api/internal/providers/anthropicwire"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// This file is the ZCode-specific half of the Anthropic Messages wire: the
// framing, the SSE→chat.completion.chunk translation and the OpenAI→Anthropic
// body shape all live in internal/providers/anthropicwire, because MiniMax Code
// speaks the same protocol. What stays here is the one field only this gateway
// accepts — output_config.effort — and the "zcode" label those shared errors
// carry.

// defaultMaxTokens mirrors the shared wire default so the local assertions keep
// checking the same number as the shared implementation.
const defaultMaxTokens = anthropicwire.DefaultMaxTokens

// anthropicBody builds the request body and adds the plan gateway's reasoning
// tier. output_config.effort is the ZCode shape for GLM reasoning levels; do
// not invent levels a model does not declare.
func anthropicBody(req translate.ChatRequest, reasoningLevel string, maxTokensCap int) map[string]any {
	body := anthropicwire.Body(req, maxTokensCap)
	if reasoningLevel != "" && reasoningLevel != "none" {
		body["output_config"] = map[string]any{"effort": reasoningLevel}
	}
	return body
}

func rewriteChatStream(resp *http.Response) *http.Response {
	return anthropicwire.RewriteStream(resp, "zcode")
}

func Aggregate(reader io.Reader) (map[string]any, error) {
	return anthropicwire.Aggregate(reader, "zcode")
}

func translateAnthropicStream(src io.Reader, dst io.Writer) error {
	return anthropicwire.TranslateStream(src, dst, "zcode")
}

func reconcileFinish(finish string, sawToolUse bool) string {
	return anthropicwire.ReconcileFinish(finish, sawToolUse)
}
