package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Models returns the model catalog for an account.
//
// The remote catalog lives one level up from the chat base (/proxy/, not
// /proxy/autoclaw) - walking down from chat/completions will never find it.
// It also requires X-Version: the upstream gates the catalog on client version
// and an old value silently advertises a shorter list rather than erroring.
//
// A remote failure is not fatal: the static table is the fallback, because a
// catalog hiccup must not take the account offline.
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	if cached, ok := c.cachedCatalog(accountID); ok {
		return cached, nil
	}
	models, err := c.fetchRemoteCatalog(ctx, accountID)
	if err != nil || len(models) == 0 {
		return staticModels(), nil
	}
	c.storeCatalog(accountID, models)
	return models, nil
}

func (c *Client) cachedCatalog(accountID string) ([]providers.ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.catalogs[accountID]
	if !ok {
		return nil, false
	}
	if c.now().Sub(cached.at) > catalogCacheTTL*time.Second {
		return nil, false
	}
	return cached.models, true
}

func (c *Client) storeCatalog(accountID string, models []providers.ModelInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalogs == nil {
		c.catalogs = map[string]catalogCache{}
	}
	c.catalogs[accountID] = catalogCache{models: models, at: c.now()}
}

// fetchRemoteCatalog pulls the upstream model config.
func (c *Client) fetchRemoteCatalog(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, metadataTimeoutSeconds*time.Second)
	defer cancel()
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(c.userapiBase, "/") + catalogPath
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range signedHeaders(cred.AccessToken, c.now()) {
		req.Header.Set(name, value)
	}
	req.Header.Set("X-Request-Model", defaultRoute)
	resp, err := client.Do(req)
	if err != nil {
		return nil, c.transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, newProviderError(resp.StatusCode, "")
	}
	// The catalog response is NOT the {code,msg,data} envelope the userapi
	// endpoints use: it is a flat {"models":[...]} document. Decoding it as an
	// envelope yields code=0 (absent reads as zero) and an empty Data, so the
	// parser silently returns nothing and the caller falls back to the two
	// static models - the catalog looks "successful" while advertising almost
	// nothing. Read the raw body and let parseCatalog try the envelope shape
	// only as a secondary possibility.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read autoclaw catalog: %w", err)
	}
	if models := parseCatalog(raw); len(models) > 0 {
		return models, nil
	}
	// Envelope shape, in case a future gateway wraps it.
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("parse autoclaw catalog: %w", err)
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("autoclaw catalog rejected: code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	return parseCatalog(envelope.Data), nil
}

// parseCatalog accepts the several shapes the model config has been seen in:
// a bare array, or {models: [...]} / {list: [...]}, with either camelCase or
// snake_case capability fields.
func parseCatalog(raw json.RawMessage) []providers.ModelInfo {
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		var wrapper struct {
			Models []map[string]any `json:"models"`
			List   []map[string]any `json:"list"`
			Items  []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return nil
		}
		entries = wrapper.Models
		if len(entries) == 0 {
			entries = wrapper.List
		}
		if len(entries) == 0 {
			entries = wrapper.Items
		}
	}
	out := make([]providers.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		id := stringField(entry, "id", "model", "model_id", "modelId", "route_id", "routeId")
		if id == "" {
			continue
		}
		name := stringField(entry, "name", "display_name", "displayName", "label")
		if name == "" {
			name = id
		}
		// The catalog advertises the full route id (zai_auto); strip it for the
		// public model name so clients address models the way they always do.
		public := stripRoutePrefix(id)
		if public == "" {
			public = id
		}
		out = append(out, providers.ModelInfo{
			NativeModel: id,
			PublicModel: public,
			DisplayName: name,
			Capabilities: providers.ModelCapabilities{
				ContextWindow: intField(entry, "context_window", "contextWindow", "max_input_tokens", "maxInputTokens", "context"),
				MaxOutput:     intField(entry, "max_tokens", "maxTokens", "max_output_tokens", "maxOutputTokens", "output"),
				Tools:         true,
				Images:        boolField(entry, "supports_images", "supportsImages", "support_image", "supportImage", "images"),
				Reasoning:     boolField(entry, "supports_reasoning", "supportsReasoning", "reasoning"),
			},
		})
	}
	return out
}

func stringField(entry map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := entry[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func intField(entry map[string]any, keys ...string) int {
	for _, key := range keys {
		switch value := entry[key].(type) {
		case float64:
			return int(value)
		case json.Number:
			if parsed, err := value.Int64(); err == nil {
				return int(parsed)
			}
		case int:
			return value
		}
	}
	return 0
}

func boolField(entry map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := entry[key].(bool); ok {
			return value
		}
		// Some catalogs express capability as {"input": ["image", ...]}.
		if list, ok := entry[key].([]any); ok {
			for _, item := range list {
				if text, ok := item.(string); ok && strings.EqualFold(text, "image") {
					return true
				}
			}
		}
	}
	if list, ok := entry["input"].([]any); ok {
		for _, item := range list {
			if text, ok := item.(string); ok && strings.EqualFold(text, "image") {
				return true
			}
		}
	}
	return false
}
