package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// catalogURL is the unauthenticated catalogue endpoint the official ZCode
// client also reads. Live probes verified the shape:
//
//	{"code":0,"data":{
//	  "providers":[{"id":"zai","schema":"anthropic","baseUrl":"...",
//	    "defaultModel":"GLM-5.3","models":[{"modelId":"GLM-5.3","name":"...",
//	    "contextWindow":1000000,"maxCompletionTokens":128000}]}],
//	  "builtinModels":[{"id":"GLM-5.3", ...}]}}
const catalogURL = "https://zcode.z.ai/api/v1/client/configs"

// catalogTimeout keeps the live catalogue fetch from blocking the executor's
// model listing: the static fallback list is good enough for routing.
const catalogTimeout = 5 * time.Second

// fallbackCatalog mirrors the coding-plan catalogue last seen live. Keep in
// sync with the GLM release cadence; the live endpoint wins whenever it is
// reachable.
var fallbackCatalog = []providers.ModelInfo{
	{
		NativeModel: "GLM-5.3",
		PublicModel: "GLM-5.3",
		DisplayName: "GLM-5.3",
		Capabilities: providers.ModelCapabilities{
			ContextWindow:    1_000_000,
			MaxOutput:        128_000,
			Tools:            true,
			Images:           true,
			Reasoning:        true,
			ReasoningOptions: []string{"low", "high", "max"},
			ReasoningDefault: "high",
			ReasoningType:    "effort",
		},
	},
	{
		NativeModel: "GLM-5.3-Flash",
		PublicModel: "GLM-5.3-Flash",
		DisplayName: "GLM-5.3 Flash",
		Capabilities: providers.ModelCapabilities{
			ContextWindow:    1_000_000,
			MaxOutput:        128_000,
			Tools:            true,
			Images:           true,
			Reasoning:        true,
			ReasoningOptions: []string{"low", "high", "max"},
			ReasoningDefault: "high",
			ReasoningType:    "effort",
		},
	},
	{
		NativeModel: "GLM-5.2",
		PublicModel: "GLM-5.2",
		DisplayName: "GLM-5.2",
		Capabilities: providers.ModelCapabilities{
			ContextWindow:    200_000,
			MaxOutput:        128_000,
			Tools:            true,
			Images:           true,
			Reasoning:        true,
			ReasoningOptions: []string{"low", "high", "max"},
			ReasoningDefault: "high",
			ReasoningType:    "effort",
		},
	},
	{
		NativeModel: "GLM-5-Turbo",
		PublicModel: "GLM-5-Turbo",
		DisplayName: "GLM-5 Turbo",
		Capabilities: providers.ModelCapabilities{
			ContextWindow: 200_000,
			MaxOutput:     128_000,
			Tools:         true,
			Images:        false,
			Reasoning:     true,
		},
	},
}

// Models returns the live catalogue when reachable, else the static fallback.
// accountID is unused for the unauthenticated catalogue, but kept on the
// signature so the adapter satisfies providers.ModelCatalogProvider.
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	_ = accountID
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	url := catalogURL
	if c.catalogURL != "" {
		url = c.catalogURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fallbackCatalog, nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	resp, err := c.http.Do(req)
	if err != nil {
		return fallbackCatalog, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fallbackCatalog, nil
	}
	models := decodeCatalog(resp)
	if len(models) == 0 {
		return fallbackCatalog, nil
	}
	return models, nil
}

// decodeCatalog walks the live catalogue shape and maps entries to
// ModelInfo. Unknown shapes fall through to the fallback list at the call
// site.
func decodeCatalog(resp *http.Response) []providers.ModelInfo {
	var body struct {
		Code int `json:"code"`
		Data struct {
			Providers []struct {
				ID           string `json:"id"`
				Schema       string `json:"schema"`
				BaseURL      string `json:"baseUrl"`
				DefaultModel string `json:"defaultModel"`
				Models       []struct {
					ModelID             string `json:"modelId"`
					Name                string `json:"name"`
					ContextWindow       int    `json:"contextWindow"`
					MaxCompletionTokens int    `json:"maxCompletionTokens"`
				} `json:"models"`
			} `json:"providers"`
			BuiltinModels []struct {
				// The live payload names this field modelId, exactly like the
				// per-provider models array; a bare "id" is accepted as a
				// fallback for older payloads.
				ModelID             string `json:"modelId"`
				ID                  string `json:"id"`
				Name                string `json:"name"`
				ContextWindow       int    `json:"contextWindow"`
				MaxCompletionTokens int    `json:"maxCompletionTokens"`
				Vision              bool   `json:"vision"`
				Capabilities        struct {
					Vision bool `json:"vision"`
				} `json:"capabilities"`
				Reasoning struct {
					DefaultLevel string `json:"defaultLevel"`
				} `json:"reasoning"`
			} `json:"builtinModels"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]providers.ModelInfo, 0, len(body.Data.BuiltinModels))
	appendModel := func(id, name string, ctx, maxOut int, vision bool, defaultLevel string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		if name == "" {
			name = id
		}
		reasoningDefault := strings.TrimSpace(defaultLevel)
		if reasoningDefault == "" {
			reasoningDefault = "high"
		}
		out = append(out, providers.ModelInfo{
			NativeModel: id,
			PublicModel: id,
			DisplayName: name,
			Capabilities: providers.ModelCapabilities{
				ContextWindow:    ctx,
				MaxOutput:        maxOut,
				Tools:            true,
				Images:           vision,
				Reasoning:        true,
				ReasoningOptions: []string{"low", "high", "max"},
				ReasoningDefault: reasoningDefault,
				ReasoningType:    "effort",
			},
		})
	}
	for _, p := range body.Data.Providers {
		if !strings.EqualFold(p.Schema, "anthropic") {
			continue
		}
		for _, m := range p.Models {
			appendModel(m.ModelID, m.Name, m.ContextWindow, m.MaxCompletionTokens, false, "")
		}
	}
	for _, m := range body.Data.BuiltinModels {
		id := m.ModelID
		if strings.TrimSpace(id) == "" {
			id = m.ID
		}
		vision := m.Vision || m.Capabilities.Vision
		appendModel(id, m.Name, m.ContextWindow, m.MaxCompletionTokens, vision, m.Reasoning.DefaultLevel)
	}
	return out
}
