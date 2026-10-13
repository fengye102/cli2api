package raccoon

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

// buildChatBody serializes the internal chat request for Raccoon's
// OpenAI-compatible endpoint. The upstream passes through whatever model id
// it is given, so we only set model and force stream.
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

func resolveModel(model string) string {
	clean := strings.TrimSpace(model)
	clean = strings.TrimPrefix(clean, "raccoon/")
	return strings.TrimSpace(clean)
}
