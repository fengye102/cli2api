package gateway

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// maxImageRequestBytes bounds an inbound /v1/images/generations body. Image
// requests are a prompt plus a handful of knobs, but an image-to-image call can
// carry a base64 source image, so the cap matches the chat one.
const maxImageRequestBytes = 64 << 20

// HandleImagesGenerations serves POST /v1/images/generations.
//
// The request is OpenAI-shaped and is relayed to the provider with `model`
// rewritten; the provider's answer ({"created":…,"data":[{"url":…}]}) goes back
// verbatim. Chat-shaped traffic for the same model is turned away in the
// provider with a pointer here, so the two planes never get confused.
func (h *Handler) HandleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImageRequestBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var fields struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON："+err.Error())
		return
	}
	if fields.Model == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "model required")
		return
	}
	if fields.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "prompt required")
		return
	}

	execution, err := h.PrepareChatExecution(r, translate.ChatRequest{Model: fields.Model})
	if err != nil {
		writeChatHTTPError(w, err)
		return
	}
	requestID := execution.RequestID
	started := execution.Started
	publicModel := execution.PublicModel
	w.Header().Set("X-Request-Id", requestID)

	result, err := h.Executor.GenerateImage(execution.Context, providers.ImageGenerationRequest{
		PublicModel: publicModel,
		Body:        body,
	}, execution.Prefer, execution.ProviderFilter)
	if err != nil {
		h.finishRequestLog(requestID, started, execution.Request, publicModel, result.AccountID,
			firstNonEmpty(result.Provider, execution.ProviderFilter), result.Routing,
			accounts.RequestStatusError, 0, nil, err, result.AttemptCount, "")
		WriteClassifiedErr(w, err)
		return
	}
	h.finishRequestLog(requestID, started, execution.Request, publicModel, result.AccountID,
		firstNonEmpty(result.Provider, execution.ProviderFilter), result.Routing,
		accounts.RequestStatusOK, 0, &StreamRelayStats{Credits: result.Credits, Model: publicModel},
		nil, result.AttemptCount, "")
	if result.AccountID != "" {
		w.Header().Set("X-Qoder-Account", result.AccountID)
		w.Header().Set("X-CLI2API-Account", result.AccountID)
	}
	if provider := firstNonEmpty(result.Provider, execution.ProviderFilter); provider != "" {
		w.Header().Set("X-CLI2API-Provider", provider)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Body)
}
