package runable

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// ---- fakes ----------------------------------------------------------------

type fakeStore struct {
	mu      sync.Mutex
	payload []byte
	account accounts.Account
}

func (s *fakeStore) Get(context.Context, string) (accounts.Account, error) { return s.account, nil }

func (s *fakeStore) LoadCredentialPayload(context.Context, string) (string, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return CredentialFormat, append([]byte(nil), s.payload...), nil
}

func (s *fakeStore) SaveCredentialPayload(context.Context, string, string, []byte) error { return nil }

func (s *fakeStore) Observe(context.Context, string, string, string, string, string) error {
	return nil
}

func storeWith(t *testing.T, cred Credential) *fakeStore {
	t.Helper()
	payload, err := EncodeCredential(cred)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return &fakeStore{payload: payload, account: accounts.Account{ID: "acc_runable", Provider: ProviderID}}
}

// aiSDKStream is one turn as the edge emits it: a start frame, two text deltas
// (one in the v5 spelling, one in v4), a custom frame carrying Anthropic usage,
// and the closing finish frame.
const aiSDKStream = `data: {"type":"start"}

data: {"type":"text-start","id":"0"}

data: {"type":"text-delta","id":"0","delta":"在"}

data: {"type":"text-delta","id":"0","textDelta":"的"}

data: {"type":"custom","providerMetadata":{"anthropic":{"usage":{"input_tokens":9,"output_tokens":2}}}}

data: {"type":"text-end","id":"0"}

data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":9,"outputTokens":2,"totalTokens":11}}

`

// ---- credential -----------------------------------------------------------

func TestDecodeCredentialShapes(t *testing.T) {
	t.Run("console paste box wraps the value in api_key", func(t *testing.T) {
		cred, err := DecodeCredential([]byte(`{"api_key":"Cookie: sess=xyz"}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cred.Cookie != "sess=xyz" {
			t.Fatalf("cookie = %q", cred.Cookie)
		}
	})

	t.Run("bare cookie header", func(t *testing.T) {
		cred, err := DecodeCredential([]byte("Cookie: __Secure-better-auth.session_token=abc; other=1"))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cred.Cookie != "__Secure-better-auth.session_token=abc; other=1" {
			t.Fatalf("cookie = %q", cred.Cookie)
		}
	})

	t.Run("canonical json", func(t *testing.T) {
		cred, err := DecodeCredential([]byte(`{"format":"runable-cookie-v1","cookie":"a=b; c=d","name":"主号","email":"x@y.z"}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cred.Cookie != "a=b; c=d" || cred.Name != "主号" || cred.Email != "x@y.z" {
			t.Fatalf("credential = %+v", cred)
		}
	})

	t.Run("multi-line paste is folded", func(t *testing.T) {
		cred, err := DecodeCredential([]byte("a=b\rc=d\n"))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cred.Cookie != "a=b; c=d" {
			t.Fatalf("cookie = %q", cred.Cookie)
		}
	})

	t.Run("a bearer token is not a cookie", func(t *testing.T) {
		if _, err := DecodeCredential([]byte("sk-abc123")); err == nil {
			t.Fatal("a token without name=value must be rejected")
		}
	})
}

// ---- catalog --------------------------------------------------------------

func TestParseCatalogKeepsChatModelsOnly(t *testing.T) {
	body := `{"json":{"models":[
		{"id":"runable/runner-chat","name":"Runner","type":"language","supportsChatMode":true,"supportsAgentMode":false,"isFree":true,"context_window":150000,"max_tokens":128000},
		{"id":"anthropic/claude-haiku-5.5","name":"Claude Haiku 5.5","type":"language","supportsChatMode":true,"supportsAgentMode":true,"isFree":true,"context_window":1000000,"max_tokens":64000},
		{"id":"openai/gpt-image-2","name":"Image","type":"image","supportsChatMode":false,"isFree":false},
		{"id":"x/agent-only","name":"","type":"language","supportsChatMode":false,"isFree":false},
		{"id":"z/paid","name":"Paid","type":"language","supportsChatMode":true,"isFree":false,"context_window":150000,"max_tokens":32000}
	]}}`
	models, err := parseCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("got %d models, want 3: %+v", len(models), models)
	}
	byID := map[string]int{}
	for i, model := range models {
		byID[model.PublicModel] = i
	}
	for _, want := range []string{"runable/runner-chat", "anthropic/claude-haiku-5.5", "z/paid"} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("%s missing from %+v", want, byID)
		}
	}
	runner := models[byID["runable/runner-chat"]]
	if !runner.Free {
		t.Error("runner-chat should be marked free")
	}
	if runner.Capabilities.ContextWindow != 150000 || runner.Capabilities.MaxOutput != 128000 {
		t.Errorf("runner limits = %+v", runner.Capabilities)
	}
	if runner.Capabilities.Tools {
		t.Error("the chat endpoint has no tool channel; Tools must be false")
	}
	if !strings.EqualFold(runner.DisplayName, "Runner") {
		t.Errorf("display name = %q", runner.DisplayName)
	}
}

// ---- prompt ---------------------------------------------------------------

func TestBuildPromptKeepsSystemVerbatimAndLabelsTurns(t *testing.T) {
	prompt := buildPrompt([]translate.ChatMessage{
		{Role: "system", Content: "You are Hermes Agent. 不要改这句话。"},
		{Role: "user", Content: "第一问"},
		{Role: "assistant", Content: "第一答"},
		{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "看图"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
		}},
		{Role: "tool", Content: "工具结果"},
	})
	want := "You are Hermes Agent. 不要改这句话。\n\n第一问\n\n[assistant]: 第一答\n\n看图\n[image omitted: this endpoint is text only]\n\n[tool]: 工具结果"
	if prompt != want {
		t.Fatalf("prompt =\n%q\nwant\n%q", prompt, want)
	}
}

// ---- chat -----------------------------------------------------------------

func TestChatStreamRelaysAISDKFrames(t *testing.T) {
	var seenCookie, seenDevice, seenModel atomicStrings
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ModelsPath:
			io.WriteString(w, `{"json":{"models":[{"id":"runable/runner-chat","name":"Runner","type":"language","supportsChatMode":true,"isFree":true}]}}`)
		case ChatPath:
			seenCookie.set(r.Header.Get("Cookie"))
			seenDevice.set(r.Header.Get("x-runable-device"))
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			seenModel.set(str(body["modelId"]))
			if str(body["trigger"]) != "submit-message" {
				t.Errorf("trigger = %v", body["trigger"])
			}
			if msg, ok := body["message"].(map[string]any); ok {
				parts, _ := msg["parts"].([]any)
				if len(parts) != 1 {
					t.Errorf("parts = %v", parts)
				} else if part, ok := parts[0].(map[string]any); ok {
					if text, _ := part["text"].(string); !strings.Contains(text, "你好") {
						t.Errorf("prompt = %q", text)
					}
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, aiSDKStream)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(storeWith(t, Credential{Cookie: "sess=abc"}))
	client.baseURL = srv.URL

	resp, _, err := client.ChatStream(context.Background(), "acc_runable", translate.ChatRequest{
		Model:    "runable/runner-chat",
		Messages: []translate.ChatMessage{{Role: "user", Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if !strings.Contains(text, `"content":"在"`) || !strings.Contains(text, `"content":"的"`) {
		t.Fatalf("text deltas missing:\n%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason missing:\n%s", text)
	}
	if !strings.Contains(text, `"total_tokens":11`) {
		t.Fatalf("usage chunk missing:\n%s", text)
	}
	if !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("[DONE] missing:\n%s", text)
	}
	if seenCookie.get() != "sess=abc" {
		t.Errorf("cookie = %q", seenCookie.get())
	}
	if seenDevice.get() != DeviceHeader {
		t.Errorf("device header = %q", seenDevice.get())
	}
	if seenModel.get() != "runable/runner-chat" {
		t.Errorf("modelId = %q", seenModel.get())
	}
}

func TestChatNonStreamAssemblesTheStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ModelsPath {
			io.WriteString(w, `{"json":{"models":[{"id":"runable/runner-chat","type":"language","supportsChatMode":true}]}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, aiSDKStream)
	}))
	defer srv.Close()
	client := NewClient(storeWith(t, Credential{Cookie: "sess=abc"}))
	client.baseURL = srv.URL

	outcome, err := client.ChatNonStream(context.Background(), "acc_runable", translate.ChatRequest{
		Model:    "runable/runner-chat",
		Messages: []translate.ChatMessage{{Role: "user", Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("ChatNonStream: %v", err)
	}
	if outcome.Content != "在的" {
		t.Fatalf("content = %q", outcome.Content)
	}
	if outcome.FinishReason != "stop" {
		t.Fatalf("finish = %q", outcome.FinishReason)
	}
	if outcome.PromptTokens != 9 || outcome.CompletionTokens != 2 {
		t.Fatalf("usage = %d/%d", outcome.PromptTokens, outcome.CompletionTokens)
	}
}

func TestMidStreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"delta\":\"半\"}\n\ndata: {\"type\":\"error\",\"errorText\":\"OUT_OF_CREDITS\"}\n\n")
	}))
	defer srv.Close()
	client := NewClient(storeWith(t, Credential{Cookie: "sess=abc"}))
	client.baseURL = srv.URL
	if _, err := client.ChatNonStream(context.Background(), "acc_runable", translate.ChatRequest{
		Model:    "runable/runner-chat",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("an error frame must fail the turn, not return a half answer silently")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"dead session", 401, `{"message":"You are not authenticated"}`, accounts.KindAuth},
		{"session code", 401, `{"code":"SESSION_NOT_FOUND"}`, accounts.KindAuth},
		{"spent quota", 402, `{"code":"OUT_OF_CREDITS","message":"out of credits"}`, accounts.KindQuota},
		{"rate", 429, `{"message":"You are sending messages too quickly"}`, accounts.KindRateLimit},
		{"bad model", 404, `{"message":"model not found"}`, accounts.KindModelNotAvailable},
		{"bad request", 400, `{"message":"messages 为空"}`, accounts.KindInvalidRequest},
		{"edge down", 503, `<html>cloudflare</html>`, accounts.KindUnavailable},
	}
	for _, test := range cases {
		if got := Classify(test.status, test.body); got.Kind != test.want {
			t.Errorf("%s: kind = %q, want %q", test.name, got.Kind, test.want)
		}
	}
}

func TestModelsCachesAndRejectsADeadSession(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if r.Header.Get("Cookie") == "sess=dead" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"You are not authenticated"}`)
			return
		}
		io.WriteString(w, `{"json":{"models":[{"id":"runable/runner-chat","type":"language","supportsChatMode":true,"isFree":true}]}}`)
	}))
	defer srv.Close()

	client := NewClient(storeWith(t, Credential{Cookie: "sess=alive"}))
	client.baseURL = srv.URL
	if _, err := client.Models(context.Background(), "acc_runable"); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if _, err := client.Models(context.Background(), "acc_runable"); err != nil {
		t.Fatalf("second Models: %v", err)
	}
	mu.Lock()
	first := calls
	mu.Unlock()
	if first != 1 {
		t.Fatalf("catalog calls = %d, want 1 (the second call must hit the cache)", first)
	}

	dead := NewClient(storeWith(t, Credential{Cookie: "sess=dead"}))
	dead.baseURL = srv.URL
	if _, err := dead.Models(context.Background(), "acc_runable"); err == nil {
		t.Fatal("a dead session must not produce a catalog")
	}
}

// atomicStrings keeps the header assertions race-free without pulling in a
// helper package.
type atomicStrings struct {
	mu     sync.Mutex
	values []string
}

func (a *atomicStrings) set(value string) {
	a.mu.Lock()
	a.values = append(a.values, value)
	a.mu.Unlock()
}

func (a *atomicStrings) get() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.values) == 0 {
		return ""
	}
	return a.values[len(a.values)-1]
}

var _ = time.Now
