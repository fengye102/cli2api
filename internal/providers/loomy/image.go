package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Image generation.
//
// The gateway's model list carries three records with `"type":"image"`
// (Hy-Image-3.5-preview, doubao-seedream-5-lite, qwen-image-3.0-pro) and
// `capabilities.output_modalities:["image"]`. They are not chat models: a chat
// call naming one is turned away upstream with 404 "该模型暂未开放". What serves
// them is POST {modelBase}/images/generations, OpenAI-shaped in and out:
//
//	→ {"model":"Hy-Image-3.5-preview","prompt":"…","n":1,"size":"1024x1024"}
//	← {"created":1793948356,"data":[{"url":"https://…png?q-signature=…"}],
//	   "points_consumed":110}
//
// The signed URL expires (the signature carries a 12-hour window), so the body
// is relayed as-is rather than re-hosted: a client that wants a durable copy
// fetches it immediately.
const imageGenerationPath = "/images/generations"

// imageTimeout is per attempt. A generation took ~15s when measured; the
// ceiling is generous because the upstream queues behind other work and the
// client is already waiting on a one-shot request.
const imageTimeout = 180 * time.Second

// GenerateImage forwards one image request on one account.
func (c *Client) GenerateImage(ctx context.Context, accountID string, req providers.ImageGenerationRequest) (providers.ImageGenerationResult, error) {
	native := resolveModel(req.PublicModel)
	if native == "" {
		return providers.ImageGenerationResult{}, &providers.Error{
			Kind:    accounts.KindInvalidRequest,
			Message: "loomy image request is missing a model",
		}
	}
	body, err := imageRequestBody(native, req.Body)
	if err != nil {
		return providers.ImageGenerationResult{}, err
	}

	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.ImageGenerationResult{}, err
	}
	client, err := c.modelClient(ctx, accountID)
	if err != nil {
		return providers.ImageGenerationResult{}, err
	}

	callCtx, cancel := context.WithTimeout(ctx, imageTimeout)
	defer cancel()
	endpoint := strings.TrimRight(c.modelBase, "/") + imageGenerationPath
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.ImageGenerationResult{}, &providers.Error{Kind: accounts.KindInvalidRequest, Message: err.Error()}
	}
	for key, value := range modelHeaders(cred.Session) {
		httpReq.Header.Set(key, value)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		if callCtx.Err() != nil && ctx.Err() == nil {
			return providers.ImageGenerationResult{}, &providers.Error{
				Kind:    accounts.KindUnavailable,
				Message: "loomy 图像生成超时：" + err.Error(),
			}
		}
		return providers.ImageGenerationResult{}, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Message: "loomy 图像接口不可达：" + err.Error(),
			Err:     err,
		}
	}
	// readGatewayJSON closes the body, turns >=300 into a classified provider
	// error and still catches a 200-with-error envelope.
	raw, err := readGatewayJSON(resp)
	if err != nil {
		return providers.ImageGenerationResult{}, err
	}
	var payload struct {
		Data           []json.RawMessage `json:"data"`
		PointsConsumed *float64          `json:"points_consumed"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return providers.ImageGenerationResult{}, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Message: "loomy 图像接口返回了无法解析的内容",
		}
	}
	if len(payload.Data) == 0 {
		return providers.ImageGenerationResult{}, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Message: "loomy 图像接口没有返回图片",
		}
	}
	return providers.ImageGenerationResult{Body: raw, Credits: payload.PointsConsumed}, nil
}

// imageRequestBody rewrites `model` to the native name and leaves the rest of
// the client's body untouched.
func imageRequestBody(native string, body json.RawMessage) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, &providers.Error{Kind: accounts.KindInvalidRequest, Message: "请求体不是合法 JSON：" + err.Error()}
		}
	}
	model, err := json.Marshal(native)
	if err != nil {
		return nil, err
	}
	fields["model"] = model
	if len(bytes.TrimSpace(fields["prompt"])) == 0 {
		return nil, &providers.Error{Kind: accounts.KindInvalidRequest, Message: "prompt 不能为空"}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("loomy image request: %w", err)
	}
	return out, nil
}
