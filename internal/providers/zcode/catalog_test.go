package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

func TestModels_LiveCatalogue(t *testing.T) {
	body := map[string]any{
		"code": 0,
		"data": map[string]any{
			"providers": []map[string]any{
				{
					"id":     "zai",
					"schema": "anthropic",
					"models": []map[string]any{
						{"modelId": "GLM-5.3", "name": "GLM-5.3", "contextWindow": 1_000_000, "maxCompletionTokens": 128_000},
						{"modelId": "GLM-5.3-Flash", "name": "GLM-5.3 Flash", "contextWindow": 1_000_000, "maxCompletionTokens": 128_000},
					},
				},
				{
					"id":     "non-anthropic",
					"schema": "openai",
					"models": []map[string]any{
						{"modelId": "should-be-ignored"},
					},
				},
			},
			"builtinModels": []map[string]any{
				// The live payload names the id field modelId, same as the
				// per-provider models array, and carries vision under
				// capabilities plus a reasoning.defaultLevel.
				{"modelId": "GLM-5.4-Test", "name": "GLM-5.4-Test", "contextWindow": 200_000,
					"maxCompletionTokens": 128_000, "capabilities": map[string]any{"vision": true},
					"reasoning": map[string]any{"defaultLevel": "max"}},
				// Provider list wins on id conflict: the builtin entry below
				// shares an id with a provider model and must be ignored.
				{"modelId": "GLM-5.3", "name": "IGNORED-duplicate", "contextWindow": 1,
					"maxCompletionTokens": 1, "capabilities": map[string]any{"vision": false}},
			},
		},
	}
	raw, _ := json.Marshal(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()

	// decodeCatalog is the function under test; swap the URL by feeding the
	// httptest response through a synthetic http.Response.
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	models := decodeCatalog(resp)
	if len(models) != 3 {
		t.Fatalf("len(models)=%d want 3 (deduped, anthropic-only)", len(models))
	}
	ids := map[string]providers.ModelInfo{}
	for _, m := range models {
		ids[m.NativeModel] = m
	}
	glm53, ok := ids["GLM-5.3"]
	if !ok {
		t.Fatalf("GLM-5.3 missing")
	}
	if glm53.Capabilities.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow=%d (provider list should win over builtinModels on id conflict)", glm53.Capabilities.ContextWindow)
	}
	if glm53.Capabilities.MaxOutput != 128_000 {
		t.Errorf("MaxOutput=%d", glm53.Capabilities.MaxOutput)
	}
	if glm53.DisplayName == "IGNORED-duplicate" {
		t.Errorf("builtinModels duplicate overwrote provider entry")
	}
	if !glm53.Capabilities.Reasoning {
		t.Errorf("Reasoning=false")
	}
	if builtin, ok := ids["GLM-5.4-Test"]; !ok {
		t.Errorf("builtinModels entry missing (field is modelId in the live payload)")
	} else {
		if !builtin.Capabilities.Images {
			t.Errorf("GLM-5.4-Test Images=false, want true (capabilities.vision)")
		}
		if builtin.Capabilities.ReasoningDefault != "max" {
			t.Errorf("GLM-5.4-Test ReasoningDefault=%q, want max (reasoning.defaultLevel)",
				builtin.Capabilities.ReasoningDefault)
		}
	}
	if _, ok := ids["should-be-ignored"]; ok {
		t.Errorf("non-anthropic schema entry should be filtered out")
	}
}

func TestModels_FallbackOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	// Override the module-level URL by spinning up a client whose transport
	// always points at the broken server. Simpler: call decodeCatalog on a
	// 500 response and verify it returns nothing (fallback happens in Models).
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	// decodeCatalog on a non-JSON body returns nil; Models falls back.
	models := decodeCatalog(resp)
	if len(models) != 0 {
		t.Errorf("decodeCatalog on 500 should return empty, got %d", len(models))
	}
}

func TestFallbackCatalogHasCoreModels(t *testing.T) {
	ids := map[string]bool{}
	for _, m := range fallbackCatalog {
		ids[m.NativeModel] = true
		if m.PublicModel == "" {
			t.Errorf("PublicModel empty for %q", m.NativeModel)
		}
		if m.Capabilities.ContextWindow == 0 {
			t.Errorf("ContextWindow empty for %q", m.NativeModel)
		}
		if !m.Capabilities.Reasoning {
			t.Errorf("Reasoning=false for %q", m.NativeModel)
		}
	}
	for _, want := range []string{"GLM-5.3", "GLM-5.3-Flash", "GLM-5.2", "GLM-5-Turbo"} {
		if !ids[want] {
			t.Errorf("fallback catalog missing %q", want)
		}
	}
}

func TestModels_ReturnsFallbackWhenUnreachable(t *testing.T) {
	// Point the client at an unroutable server; Models must return the
	// static fallback instead of an error so the executor's catalog refresh
	// never hard-fails.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	url := server.URL
	server.Close() // no listener; every request errors immediately
	client := NewClient(nil)
	client.catalogURL = url
	models, err := client.Models(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("Models returned error: %v", err)
	}
	if len(models) == 0 {
		t.Errorf("Models returned empty list; fallback should always serve")
	}
}

func TestModels_LiveCatalogueViaClient(t *testing.T) {
	body := map[string]any{
		"code": 0,
		"data": map[string]any{
			"providers": []map[string]any{
				{
					"id":     "zai",
					"schema": "anthropic",
					"models": []map[string]any{
						{"modelId": "GLM-5.3", "name": "GLM-5.3", "contextWindow": 1_000_000, "maxCompletionTokens": 128_000},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	client := NewClient(nil)
	client.catalogURL = server.URL
	models, err := client.Models(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].NativeModel != "GLM-5.3" {
		t.Errorf("models=%+v", models)
	}
}
