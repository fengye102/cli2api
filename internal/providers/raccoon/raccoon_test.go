package raccoon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

func TestDecodeCredentialAliases(t *testing.T) {
	cred, err := DecodeCredential([]byte(`{"format":"raccoon-credential-v1","accessToken":"tok-1","refreshToken":"ref-1"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cred.AccessToken != "tok-1" || cred.RefreshToken != "ref-1" {
		t.Fatalf("cred = %+v", cred)
	}
	if !cred.Ready() {
		t.Error("ready should be true")
	}
}

func TestDecodeCredentialBearerTrim(t *testing.T) {
	cred, err := DecodeCredential([]byte(`{"token":"Bearer abc"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cred.AccessToken != "abc" {
		t.Fatalf("access token = %q, want abc", cred.AccessToken)
	}
}

func TestValidateCredential(t *testing.T) {
	if err := ValidateCredential([]byte(`{"access_token":"a","refresh_token":"b"}`)); err != nil {
		t.Errorf("valid cred rejected: %v", err)
	}
	if err := ValidateCredential([]byte(`{}`)); err == nil {
		t.Error("empty cred should be rejected")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{401, "auth"},
		{403, "auth"},
		{429, "rate_limit"},
		{500, "unavailable"},
	}
	for _, c := range cases {
		got := Classify(c.status, "boom")
		if got.Kind != c.want {
			t.Errorf("Classify(%d).Kind = %q, want %q", c.status, got.Kind, c.want)
		}
	}
}

func TestCollectChatSSE(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"你好"},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":"世界"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	outcome, err := collectChatSSE(strings.NewReader(body), "raccoon-8c4485")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if outcome.Content != "你好世界" {
		t.Fatalf("content = %q", outcome.Content)
	}
	if outcome.FinishReason != "stop" {
		t.Fatalf("finish = %q", outcome.FinishReason)
	}
}

func TestCollectChatSSEToolCalls(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"loc\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}

data: [DONE]
`
	outcome, err := collectChatSSE(strings.NewReader(body), "m")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(outcome.ToolCalls) == 0 {
		t.Fatal("tool calls should be present")
	}
	var calls []map[string]any
	if err := json.Unmarshal(outcome.ToolCalls, &calls); err != nil {
		t.Fatalf("unmarshal tool calls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	fn, _ := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("function name = %v", fn["name"])
	}
}

// memStore implements Store for tests.
type memStore struct {
	items map[string][]byte
	acc   map[string]string // accountID -> proxy
	// formats/observed record what the login path wrote, so tests can assert on
	// the stored format and the reported status.
	formats  map[string]string
	observed map[string]string
}

func (s *memStore) Get(ctx context.Context, id string) (accounts.Account, error) {
	acct := accounts.Account{ID: id, Provider: "raccoon"}
	if s.acc != nil {
		acct.ProxyURL = s.acc[id]
	}
	return acct, nil
}

func (s *memStore) LoadCredentialPayload(ctx context.Context, accountID string) (string, []byte, error) {
	format := "raccoon-credential-v1"
	if s.formats != nil && s.formats[accountID] != "" {
		format = s.formats[accountID]
	}
	return format, s.items[accountID], nil
}

func (s *memStore) SaveCredentialPayload(ctx context.Context, accountID, format string, payload []byte) error {
	s.items[accountID] = payload
	if s.formats == nil {
		s.formats = map[string]string{}
	}
	s.formats[accountID] = format
	return nil
}

func (s *memStore) Observe(ctx context.Context, id, remoteUID, status, lastError, lastKind string) error {
	if s.observed == nil {
		s.observed = map[string]string{}
	}
	s.observed[id] = status
	return nil
}

func newTestClient(t *testing.T, llmHandler http.HandlerFunc, authHandler http.HandlerFunc) (*Client, *memStore) {
	t.Helper()
	llm := httptest.NewServer(llmHandler)
	auth := httptest.NewServer(authHandler)
	t.Cleanup(llm.Close)
	t.Cleanup(auth.Close)
	store := &memStore{items: map[string][]byte{
		"acc1": []byte(`{"access_token":"tok-1","refresh_token":"ref-1"}`),
	}}
	c := NewClient(store)
	c.http = llm.Client()
	c.llmBase = llm.URL
	c.authBase = auth.URL
	return c, store
}

func TestChatStreamRelaysSSE(t *testing.T) {
	llmHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("auth = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}
	c, _ := newTestClient(t, llmHandler, nil)
	resp, _, err := c.ChatStream(context.Background(), "acc1", mustChatReq(t, "raccoon-8c4485"))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestChatStreamRefreshesOn401(t *testing.T) {
	var llmCalls, authCalls int
	llmHandler := func(w http.ResponseWriter, r *http.Request) {
		llmCalls++
		if llmCalls == 1 {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"token expired"}`)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer new-tok" {
			t.Errorf("retry auth = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}
	authHandler := func(w http.ResponseWriter, r *http.Request) {
		authCalls++
		if r.URL.Path != "/refresh" {
			t.Errorf("auth path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"new-tok","refresh_token":"ref-2"}`)
	}
	c, store := newTestClient(t, llmHandler, authHandler)
	resp, _, err := c.ChatStream(context.Background(), "acc1", mustChatReq(t, "raccoon-8c4485"))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	resp.Body.Close()
	if llmCalls != 2 {
		t.Errorf("llm calls = %d, want 2", llmCalls)
	}
	if authCalls != 1 {
		t.Errorf("auth calls = %d, want 1", authCalls)
	}
	saved := string(store.items["acc1"])
	if !strings.Contains(saved, "new-tok") {
		t.Errorf("persisted credential = %q, want new-tok", saved)
	}
}

func TestModelsFallbackWithoutCredential(t *testing.T) {
	c := NewClient(&memStore{items: map[string][]byte{}})
	models, err := c.Models(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 5 {
		t.Fatalf("fallback models = %d, want 5", len(models))
	}
}

// TestModelsRemoteCatalogCategories pins the live /model_catalog shape: `data`
// is an object carrying categories[].models[], not a flat array. Parsing the
// array form only is what broke the account probe in production.
func TestModelsRemoteCatalogCategories(t *testing.T) {
	llmHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model_catalog" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"categories":[
			{"type":"chat","default_model":"raccoon-x","models":[
				{"name":"raccoon-x","displayName":"Raccoon X","description":"Raccoon-Work",
				 "tags":["vision"],"context_window":500000,"max_tokens":90000,
				 "billing_multiplier":1,"billing_effective_multiplier":0.5}
			]},
			{"type":"chat","models":[
				{"name":"sn-flash","display_name":"SenseNova Flash","params":{"context_window":256000,"max_tokens":63999}}
			]}
		]}}`)
	}
	c, _ := newTestClient(t, llmHandler, nil)
	models, err := c.Models(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d (%+v), want 2", len(models), models)
	}
	if models[0].NativeModel != "raccoon-x" || models[0].DisplayName != "Raccoon X" {
		t.Fatalf("models[0] = %+v", models[0])
	}
	// snake_case fields nested under `params` must still resolve.
	if models[1].NativeModel != "sn-flash" || models[1].DisplayName != "SenseNova Flash" {
		t.Fatalf("models[1] = %+v", models[1])
	}
	if models[1].Capabilities.ContextWindow != 256000 || models[1].Capabilities.MaxOutput != 63999 {
		t.Fatalf("params caps = %+v", models[1].Capabilities)
	}
	// Vision is inferred from tags when there is no explicit boolean.
	if !models[0].Capabilities.Images || models[1].Capabilities.Images {
		t.Fatalf("images = %+v", []bool{models[0].Capabilities.Images, models[1].Capabilities.Images})
	}
	// Effective (post-promotion) multiplier wins over the list price.
	if models[0].Credits != "x0.5 credits" {
		t.Fatalf("credits = %q, want x0.5 credits", models[0].Credits)
	}
	if models[1].Credits != "" {
		t.Fatalf("credits = %q, want empty when the catalog gives none", models[1].Credits)
	}
}

// TestModelsRemoteCatalogDeduplicatesCategories covers repeated ids across
// categories: the first occurrence wins, matching the upstream client.
func TestModelsRemoteCatalogDeduplicatesCategories(t *testing.T) {
	llmHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"categories":[
			{"models":[{"name":"dup","displayName":"First"}]},
			{"models":[{"name":"dup","displayName":"Second"},{"name":"","displayName":"nameless"}]}
		]}}`)
	}
	c, _ := newTestClient(t, llmHandler, nil)
	models, err := c.Models(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v, want 1 (dedup + drop nameless)", models)
	}
	if models[0].DisplayName != "First" {
		t.Fatalf("display = %q, want First", models[0].DisplayName)
	}
}

// TestModelsLiveCatalogFixture parses a captured production /model_catalog
// response. This is the exact shape that broke the account probe: `data` is an
// object, models live under categories[].models[], and the numeric fields are
// snake_case inside `params`. The fixture is real upstream output, so a future
// change to the parser cannot silently regress against it.
func TestModelsLiveCatalogFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "model_catalog_live.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	llmHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model_catalog" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}
	c, _ := newTestClient(t, llmHandler, nil)
	models, err := c.Models(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 9 {
		t.Fatalf("models = %d, want 9", len(models))
	}
	byID := map[string]providers.ModelInfo{}
	for _, m := range models {
		byID[m.NativeModel] = m
	}

	work, ok := byID["raccoon-8c4485"]
	if !ok {
		t.Fatal("raccoon-8c4485 missing")
	}
	if work.DisplayName != "Raccoon-Work" {
		t.Fatalf("display = %q", work.DisplayName)
	}
	// Numeric fields come from params.context_window / params.max_tokens.
	if work.Capabilities.ContextWindow != 1000000 || work.Capabilities.MaxOutput != 100000 {
		t.Fatalf("caps = %+v", work.Capabilities)
	}
	// Vision is inferred from the tags array.
	if !work.Capabilities.Images {
		t.Fatal("raccoon-8c4485 should report image support via tags")
	}

	// A limited-free model carries an explicit zero effective multiplier.
	if lite, ok := byID["sn-sensenova-6-8-flash"]; ok {
		if !lite.Free {
			t.Fatalf("sn-sensenova-6-8-flash should be free: %+v", lite)
		}
		if lite.Credits != "" {
			t.Fatalf("free model credits = %q, want empty", lite.Credits)
		}
	} else {
		t.Fatal("sn-sensenova-6-8-flash missing")
	}

	// A discounted model reports its effective multiplier.
	if glm, ok := byID["sn-glm-5-3"]; ok {
		if glm.Credits != "x0.75 credits" {
			t.Fatalf("credits = %q, want x0.75 credits", glm.Credits)
		}
	} else {
		t.Fatal("sn-glm-5-3 missing")
	}
}

func TestModelsRemoteCatalog(t *testing.T) {
	llmHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model_catalog" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[
			{"name":"raccoon-x","displayName":"Raccoon X","contextWindow":500000,"maxTokens":90000,"supportImage":true,"supportThinking":true}
		]}`)
	}
	c, _ := newTestClient(t, llmHandler, nil)
	models, err := c.Models(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 1 || models[0].NativeModel != "raccoon-x" {
		t.Fatalf("models = %+v", models)
	}
	if !models[0].Capabilities.Images || !models[0].Capabilities.Reasoning {
		t.Fatalf("caps = %+v", models[0].Capabilities)
	}
}

func mustChatReq(t *testing.T, model string) translate.ChatRequest {
	t.Helper()
	return translate.ChatRequest{
		Model:    model,
		Messages: []translate.ChatMessage{{Role: "user", Content: []byte(`"hi"`)}},
	}
}
