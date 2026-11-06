package loomy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

// buildChatBody serializes the internal chat request for the gateway. The
// gateway is OpenAI-compatible and passes unknown members through, so the body
// is the caller's own body with `model` rewritten to the native id and `stream`
// forced on (the adapter always collects or relays an SSE stream).
func buildChatBody(req translate.ChatRequest, model string) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return nil, err
	}
	obj["model"] = model
	obj["stream"] = true
	return json.Marshal(obj)
}

// resolveModel maps a public model id onto the gateway's own spelling. Both
// "loomy/qwen3.8-flash" and "qwen3.8-flash" resolve to qwen3.8-flash.
func resolveModel(model string) string {
	clean := strings.TrimSpace(model)
	clean = strings.TrimPrefix(clean, publicModelPrefix)
	clean = strings.TrimPrefix(clean, "loomy:")
	return strings.TrimSpace(clean)
}

// publicModel is the id this gateway advertises for a native model.
func publicModel(native string) string {
	native = strings.TrimSpace(native)
	if native == "" {
		return ""
	}
	return publicModelPrefix + native
}

// modelHeaders is the header set every model-plane call carries.
//
// Two auth spellings go out at once because the client's provider sets both and
// the gateway accepts either; sending only one works today but is a needless
// coin flip. traceparent is not optional: without it the gateway accepts the
// connection and then never answers, which is indistinguishable from a hang.
func modelHeaders(session string) map[string]string {
	session = strings.TrimSpace(session)
	return map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + session,
		"token":         session,
		"traceparent":   "00-" + randomHex(32) + "-" + randomHex(16) + "-01",
		"loomy-version": loomyVersion,
	}
}

// streamHeaders is modelHeaders for an SSE call.
func streamHeaders(session string) map[string]string {
	headers := modelHeaders(session)
	headers["Accept"] = "text/event-stream"
	return headers
}
