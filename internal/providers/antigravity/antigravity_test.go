package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

func init() {
	acceptChatBase = func(parsed *url.URL) bool {
		if parsed == nil || parsed.Host == "" {
			return false
		}
		if strings.EqualFold(parsed.Scheme, "https") {
			return true
		}
		return parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost")
	}
}

func TestCredentialRoundTripAndBase(t *testing.T) {
	credential, err := DecodeCredential([]byte(`{"apiKey":" Bearer sk-antigravity ","base_url":"https://generativelanguage.googleapis.com/v1beta/openai/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if credential.APIKey != "sk-antigravity" || credential.Format != CredentialFormat {
		t.Fatalf("decoded credential = %+v", credential)
	}
	base, err := credential.ChatBase()
	if err != nil || base != BaseURL {
		t.Fatalf("chat base = %q err=%v", base, err)
	}
	if err := ValidateCredential(mustJSON(t, credential)); err != nil {
		t.Fatalf("valid credential rejected: %v", err)
	}
	credential.BaseURL = "http://open.bigmodel.cn/api/paas/v4"
	if err := ValidateCredential(mustJSON(t, credential)); err == nil {
		t.Fatal("http base url must be rejected")
	}
	if err := ValidateCredential([]byte(`{}`)); err == nil {
		t.Fatal("empty credential must be rejected")
	}
}

func TestEndpointKeepsV1BetaOpenAI(t *testing.T) {
	got := endpoint(BaseURL, PathChat)
	want := BaseURL + "/chat/completions"
	if got != want {
		t.Fatalf("chat endpoint = %q, want %q", got, want)
	}
	if got := endpoint("https://example.test", "/v1/models"); got != "https://example.test/v1/models" {
		t.Fatalf("bare origin endpoint = %q", got)
	}
}

func TestGeminiReasoningScale(t *testing.T) {
	cases := []struct {
		model string
		level string
		want  string
	}{
		{"gemini-3.1-pro", "low", "low"},
		{"gemini-3.1-pro", "medium", "medium"},
		{"gemini-3.1-pro", "xhigh", "high"},
		{"gemini-3.1-pro", "max", "high"},
		{"gemini-3.1-pro", "none", ""},
		{"claude-sonnet-5.5", "high", ""},
		{"gpt-oss-120b", "high", ""},
	}
	for _, tc := range cases {
		body, err := buildChatBody(translate.ChatRequest{
			Model:           tc.model,
			Messages:        []translate.ChatMessage{{Role: "user", Content: "hi"}},
			ReasoningEffort: json.RawMessage(`"` + tc.level + `"`),
		}, tc.model, tc.level)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if json.Unmarshal(body, &got) != nil {
			t.Fatalf("body is not json: %s", body)
		}
		if tc.want == "" {
			if _, ok := got["reasoning_effort"]; ok {
				t.Fatalf("%s %s kept reasoning_effort", tc.model, tc.level)
			}
		} else if got["reasoning_effort"] != tc.want {
			t.Fatalf("%s %s effort = %v, want %s", tc.model, tc.level, got["reasoning_effort"], tc.want)
		}
		if got["stream"] != true {
			t.Fatalf("%s stream = %v, want true", tc.model, got["stream"])
		}
	}
}

func TestCatalogParseAndReasoningOptions(t *testing.T) {
	models, err := ParseCatalogJSON([]byte(`{"data":[{"id":"gemini-3.1-pro"},{"id":"gemini-3.1-pro"},{"id":"claude-sonnet-5.5"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2 after dedup", len(models))
	}
	if got := strings.Join(models[0].Capabilities.ReasoningOptions, ","); got != "low,medium,high" {
		t.Fatalf("gemini options = %s", got)
	}
	if models[0].Capabilities.ReasoningDefault != "medium" || models[0].Capabilities.ReasoningType != "effort" {
		t.Fatalf("gemini caps = %+v", models[0].Capabilities)
	}
	if len(models[1].Capabilities.ReasoningOptions) != 0 {
		t.Fatalf("claude must not declare effort options: %+v", models[1].Capabilities)
	}
	// Empty upstream falls back to the curated static list.
	static := catalogModelInfos()
	if len(static) != len(catalog) {
		t.Fatalf("static models = %d, want %d", len(static), len(catalog))
	}
}

func TestChatStreamPostsBearerAndRelaysSSE(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n")
	}))
	defer upstream.Close()

	store := &memoryStore{payload: mustJSON(t, Credential{APIKey: "sk-live", BaseURL: upstream.URL + "/v1beta/openai"})}
	client := NewClient(store)
	resp, resolved, err := client.ChatStream(context.Background(), "acc", translate.ChatRequest{
		Model:           "gemini-3.1-pro",
		Messages:        []translate.ChatMessage{{Role: "user", Content: "hi"}},
		ReasoningEffort: json.RawMessage(`"medium"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer sk-live" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotPath != "/v1beta/openai/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["reasoning_effort"] != "medium" || gotBody["stream"] != true {
		t.Fatalf("outbound body = %#v", gotBody)
	}
	if resolved.ReasoningLevel != "medium" {
		t.Fatalf("resolved level = %q", resolved.ReasoningLevel)
	}
	outcome, err := collectChatSSE(resp.Body, "gemini-3.1-pro")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Content != "ok" || outcome.PromptTokens != 3 || outcome.CompletionTokens != 1 {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestChatNonStreamCollectsToolCalls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"go\\\"}\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\"}}]}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n")
	}))
	defer upstream.Close()

	store := &memoryStore{payload: mustJSON(t, Credential{APIKey: "sk-live", BaseURL: upstream.URL})}
	outcome, err := NewClient(store).ChatNonStream(context.Background(), "acc", translate.ChatRequest{
		Model:    "gemini-3.1-pro",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.PromptTokens != 10 || outcome.CompletionTokens != 4 {
		t.Fatalf("usage = %+v", outcome)
	}
	var calls []map[string]any
	if json.Unmarshal(outcome.ToolCalls, &calls) != nil || len(calls) != 1 {
		t.Fatalf("tool calls = %s", outcome.ToolCalls)
	}
	if calls[0]["id"] != "call-1" || calls[0]["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("tool call = %#v", calls[0])
	}
}

func TestModelsFetchAndCache(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"data":[{"id":"gemini-3.1-pro"},{"id":"claude-sonnet-5.5"}]}`)
	}))
	defer upstream.Close()

	store := &memoryStore{payload: mustJSON(t, Credential{APIKey: "sk-live", BaseURL: upstream.URL})}
	client := NewClient(store)
	first, err := client.Models(context.Background(), "acc")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("models = %d, want 2", len(first))
	}
	second, err := client.Models(context.Background(), "acc")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 {
		t.Fatalf("cached models = %d, want 2", len(second))
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (cached)", calls)
	}
}

func TestProbeAndQuota(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"gemini-3.1-pro"}]}`)
	}))
	defer upstream.Close()

	store := &memoryStore{payload: mustJSON(t, Credential{APIKey: "sk-live", BaseURL: upstream.URL})}
	client := NewClient(store)
	health, err := client.Probe(context.Background(), "acc")
	if err != nil {
		t.Fatal(err)
	}
	if !health.Ready || !health.Hot {
		t.Fatalf("health = %+v", health)
	}
	info, err := client.Quota(context.Background(), "acc")
	if err != nil {
		t.Fatal(err)
	}
	if info.ProviderID != "antigravity" || info.Exceeded {
		t.Fatalf("quota = %+v", info)
	}
}

func TestClassifyMapsAntigravityErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   string
	}{
		{401, `{"error":"invalid api key","authorization":"Bearer sk-secret"}`, accounts.KindAuth},
		{403, "permission denied", accounts.KindAuth},
		{429, "rate limit exceeded", accounts.KindRateLimit},
		{402, "insufficient balance", accounts.KindQuota},
		{400, "context too large", accounts.KindInvalidRequest},
		{404, "model not found", accounts.KindModelNotAvailable},
		{502, "upstream error", accounts.KindUnavailable},
	}
	for _, tc := range cases {
		got := Classify(tc.status, tc.body)
		if got.Kind != tc.kind {
			t.Fatalf("status %d kind = %s, want %s (msg %q)", tc.status, got.Kind, tc.kind, got.Message)
		}
		if strings.Contains(got.Message, "sk-secret") {
			t.Fatalf("status %d leaked secret: %q", tc.status, got.Message)
		}
	}
	classified := Classify(401, "unauthorized")
	if classified.Status != 401 {
		t.Fatalf("auth status = %d", classified.Status)
	}
}

func TestAdapterWiring(t *testing.T) {
	client := NewClient(&memoryStore{payload: mustJSON(t, Credential{APIKey: "sk-live"})})
	adapter := client.Adapter()
	if adapter.ID != "antigravity" {
		t.Fatalf("adapter id = %q", adapter.ID)
	}
	for _, capability := range []string{"credential", "chat", "models", "prober"} {
		if !adapter.Supports(capability) {
			t.Fatalf("adapter must support %s", capability)
		}
	}
	if adapter.Supports("login") {
		t.Fatal("antigravity has no login flow")
	}
}

type memoryStore struct {
	payload []byte
}

func (s *memoryStore) Get(context.Context, string) (accounts.Account, error) {
	return accounts.Account{}, nil
}
func (s *memoryStore) LoadCredentialPayload(context.Context, string) (string, []byte, error) {
	return CredentialFormat, append([]byte(nil), s.payload...), nil
}
func (s *memoryStore) SaveCredentialPayload(context.Context, string, string, []byte) error {
	return nil
}
func (s *memoryStore) Observe(context.Context, string, string, string, string, string) error {
	return nil
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
