package custom

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// fakeStore is the persistence surface the adapter needs, in memory.
type fakeStore struct {
	mu       sync.Mutex
	creds    map[string][]byte
	formats  map[string]string
	account  accounts.Account
	observed map[string]string
}

func newFakeStore(t *testing.T, accountID string, payload []byte) *fakeStore {
	t.Helper()
	return &fakeStore{
		creds:    map[string][]byte{accountID: payload},
		formats:  map[string]string{accountID: CredentialFormat},
		account:  accounts.Account{ID: accountID, Provider: ProviderID},
		observed: map[string]string{},
	}
}

func (s *fakeStore) Get(context.Context, string) (accounts.Account, error) {
	return s.account, nil
}

func (s *fakeStore) LoadCredentialPayload(_ context.Context, id string) (string, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, ok := s.creds[id]
	if !ok {
		return "", nil, io.EOF
	}
	return s.formats[id], payload, nil
}

func (s *fakeStore) SaveCredentialPayload(_ context.Context, id, format string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[id], s.formats[id] = payload, format
	return nil
}

func (s *fakeStore) Observe(_ context.Context, id, _, status, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed[id] = status
	return nil
}

// fakeRelay is an OpenAI-compatible upstream: /v1/models plus a streamed
// /v1/chat/completions.
type fakeRelay struct {
	mu       sync.Mutex
	models   string
	status   int
	authSeen string
	chatBody map[string]any
	chunks   []string
}

func newFakeRelay(t *testing.T) (*fakeRelay, *httptest.Server) {
	t.Helper()
	relay := &fakeRelay{
		models: `{"object":"list","data":[
			{"id":"deepseek-v4-pro","object":"model","owned_by":"relay","context_length":128000},
			{"id":"glm-5.2","object":"model"},
			{"id":"some-image-model","type":"image"}
		]}`,
		chunks: []string{
			`{"choices":[{"delta":{"role":"assistant","content":"你"}}]}`,
			`{"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relay.mu.Lock()
		relay.authSeen = r.Header.Get("Authorization")
		relay.mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
			relay.mu.Lock()
			status, body := relay.status, relay.models
			relay.mu.Unlock()
			if status >= 300 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		case "/v1/chat/completions":
			raw, _ := io.ReadAll(r.Body)
			parsed := map[string]any{}
			_ = json.Unmarshal(raw, &parsed)
			relay.mu.Lock()
			relay.chatBody = parsed
			status := relay.status
			chunks := append([]string(nil), relay.chunks...)
			relay.mu.Unlock()
			if status >= 300 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"你已用尽余额","type":"insufficient_quota"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range chunks {
				_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return relay, server
}

func credentialFor(server *httptest.Server) []byte {
	payload, _ := json.Marshal(map[string]any{
		"format":   CredentialFormat,
		"base_url": server.URL + "/v1",
		"api_key":  "sk-relay-test",
	})
	return payload
}

func newClient(t *testing.T, server *httptest.Server) (*Client, *fakeStore) {
	t.Helper()
	store := newFakeStore(t, "relay-1", credentialFor(server))
	return NewClient(store), store
}

func TestNormalizeBaseURLAcceptsPastedForms(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://relay.example.com", "https://relay.example.com/v1", false},
		{"https://relay.example.com/", "https://relay.example.com/v1", false},
		{"https://relay.example.com/v1", "https://relay.example.com/v1", false},
		{"https://relay.example.com/v1/", "https://relay.example.com/v1", false},
		{"https://relay.example.com/openai/v1", "https://relay.example.com/openai/v1", false},
		{" http://127.0.0.1:3000 ", "http://127.0.0.1:3000/v1", false},
		{"https://relay.example.com/v1?a=b", "https://relay.example.com/v1", false},
		{"relay.example.com/v1", "", true},
		{"ftp://relay.example.com", "", true},
		{"https://", "", true},
		{"http://169.254.169.254/latest", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeBaseURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%q accepted, want an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q → %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDecodeCredentialAliases(t *testing.T) {
	credential, err := DecodeCredential([]byte(`{
		"apiKey":" Bearer sk-abc ",
		"baseUrl":"https://relay.example.com/v1",
		"models":"gpt-4o, deepseek-v4-pro ,gpt-4o",
		"mapping":{"my-model":"deepseek-v4-pro"}
	}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if credential.APIKey != "sk-abc" {
		t.Fatalf("api key = %q", credential.APIKey)
	}
	if base, err := credential.ChatBase(); err != nil || base != "https://relay.example.com/v1" {
		t.Fatalf("base = %q err=%v", base, err)
	}
	if len(credential.Models) != 2 || credential.Models[1] != "deepseek-v4-pro" {
		t.Fatalf("models = %v", credential.Models)
	}
	if credential.ModelMapping["my-model"] != "deepseek-v4-pro" {
		t.Fatalf("mapping = %v", credential.ModelMapping)
	}
}

func TestValidateCredentialRejectsIncomplete(t *testing.T) {
	for _, payload := range []string{
		`{"base_url":"https://relay.example.com"}`,
		`{"api_key":"sk-x"}`,
		`{"api_key":"sk-x","base_url":"relay.example.com/v1"}`,
		`not json`,
	} {
		if err := ValidateCredential([]byte(payload)); err == nil {
			t.Fatalf("%s accepted, want an error", payload)
		}
	}
}

func TestModelsReadsUpstreamAndAppliesMapping(t *testing.T) {
	_, server := newFakeRelay(t)
	client, _ := newClient(t, server)

	models, err := client.Models(context.Background(), "relay-1")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	byID := map[string]providers.ModelInfo{}
	for _, model := range models {
		byID[model.PublicModel] = model
	}
	if _, ok := byID["deepseek-v4-pro"]; !ok {
		t.Fatalf("public models = %v", byID)
	}
	if byID["deepseek-v4-pro"].Capabilities.ContextWindow != 128000 {
		t.Fatalf("context window was dropped: %+v", byID["deepseek-v4-pro"].Capabilities)
	}
	if _, ok := byID["some-image-model"]; ok {
		t.Fatal("an image record must not be advertised as a chat model")
	}
}

func TestModelsAddsMappingKeys(t *testing.T) {
	_, server := newFakeRelay(t)
	store := newFakeStore(t, "relay-1", mustJSON(t, map[string]any{
		"base_url":      server.URL + "/v1",
		"api_key":       "sk-relay-test",
		"model_mapping": map[string]string{"fast": "glm-5.2"},
	}))
	client := NewClient(store)

	models, err := client.Models(context.Background(), "relay-1")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	found := false
	for _, model := range models {
		if model.PublicModel == "fast" {
			found = true
			if model.NativeModel != "glm-5.2" {
				t.Fatalf("mapping native = %q", model.NativeModel)
			}
		}
	}
	if !found {
		t.Fatalf("mapping key missing from catalog: %+v", models)
	}
}

func TestModelsFallBackToManualList(t *testing.T) {
	relay, server := newFakeRelay(t)
	relay.mu.Lock()
	relay.status = http.StatusInternalServerError
	relay.mu.Unlock()
	store := newFakeStore(t, "relay-1", mustJSON(t, map[string]any{
		"base_url": server.URL + "/v1",
		"api_key":  "sk-relay-test",
		"models":   []string{"deepseek-v4-pro"},
	}))
	client := NewClient(store)

	models, err := client.Models(context.Background(), "relay-1")
	if err != nil {
		t.Fatalf("Models with a manual list must not fail: %v", err)
	}
	if len(models) != 1 || models[0].PublicModel != "deepseek-v4-pro" {
		t.Fatalf("models = %+v", models)
	}
}

func TestChatNonStreamCollectsDeltas(t *testing.T) {
	relay, server := newFakeRelay(t)
	client, _ := newClient(t, server)

	outcome, err := client.ChatNonStream(context.Background(), "relay-1", translate.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []translate.ChatMessage{{Role: "developer", Content: "rules"}, {Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatNonStream: %v", err)
	}
	if outcome.Content != "你好" {
		t.Fatalf("content = %q", outcome.Content)
	}
	if outcome.PromptTokens != 7 || outcome.CompletionTokens != 2 {
		t.Fatalf("usage = %d/%d", outcome.PromptTokens, outcome.CompletionTokens)
	}
	if relay.authSeen != "Bearer sk-relay-test" {
		t.Fatalf("upstream saw Authorization %q", relay.authSeen)
	}
	if relay.chatBody["model"] != "deepseek-v4-pro" {
		t.Fatalf("upstream model = %v", relay.chatBody["model"])
	}
	if relay.chatBody["stream"] != true {
		t.Fatalf("stream was not forced: %v", relay.chatBody)
	}
	messages, _ := relay.chatBody["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role was not folded into system: %v", first)
	}
}

func TestChatUsesMappedUpstreamModelAndStripsPrefix(t *testing.T) {
	relay, server := newFakeRelay(t)
	store := newFakeStore(t, "relay-1", mustJSON(t, map[string]any{
		"base_url":      server.URL + "/v1",
		"api_key":       "sk-relay-test",
		"model_mapping": map[string]string{"fast": "glm-5.2"},
	}))
	client := NewClient(store)

	for _, requested := range []string{"fast", "custom/fast"} {
		if _, err := client.ChatNonStream(context.Background(), "relay-1", translate.ChatRequest{
			Model:    requested,
			Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
		}); err != nil {
			t.Fatalf("%s: %v", requested, err)
		}
		relay.mu.Lock()
		got := relay.chatBody["model"]
		relay.mu.Unlock()
		if got != "glm-5.2" {
			t.Fatalf("%s went upstream as %v, want glm-5.2", requested, got)
		}
	}
}

func TestChatStreamReturnsUpstreamBody(t *testing.T) {
	_, server := newFakeRelay(t)
	client, _ := newClient(t, server)

	resp, _, err := client.ChatStream(context.Background(), "relay-1", translate.ChatRequest{
		Model:    "glm-5.2",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "data: [DONE]") {
		t.Fatalf("stream body = %s", raw)
	}
}

func TestChatSurfacesUpstreamErrors(t *testing.T) {
	relay, server := newFakeRelay(t)
	relay.mu.Lock()
	relay.status = http.StatusPaymentRequired
	relay.mu.Unlock()
	client, _ := newClient(t, server)

	_, err := client.ChatNonStream(context.Background(), "relay-1", translate.ChatRequest{
		Model:    "glm-5.2",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	providerErr, ok := err.(*providers.Error)
	if !ok {
		t.Fatalf("err = %v, want a classified provider error", err)
	}
	if providerErr.Kind != accounts.KindQuota {
		t.Fatalf("kind = %q, want quota (402 is a balance problem, not the request)", providerErr.Kind)
	}
}

func TestClassifyTaxonomy(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   string
	}{
		{401, `{"error":{"message":"invalid api key"}}`, accounts.KindAuth},
		{403, `{"error":{"message":"forbidden"}}`, accounts.KindAuth},
		{402, `{"error":{"message":"insufficient balance"}}`, accounts.KindQuota},
		{200, `{"error":{"message":"额度不足"}}`, accounts.KindQuota},
		{429, `{"error":{"message":"rate limit"}}`, accounts.KindRateLimit},
		{404, `{"error":{"message":"model not found"}}`, accounts.KindModelNotAvailable},
		{400, `{"error":{"message":"messages is required"}}`, accounts.KindInvalidRequest},
		{500, `{"error":{"message":"upstream exploded"}}`, accounts.KindUnavailable},
		{503, ``, accounts.KindUnavailable},
	}
	for _, tc := range cases {
		if got := Classify(tc.status, tc.body); got.Kind != tc.kind {
			t.Fatalf("%d %s → %q, want %q", tc.status, tc.body, got.Kind, tc.kind)
		}
	}
}

func TestClassifyRedactsBearer(t *testing.T) {
	got := Classify(401, `{"error":{"message":"bad header Authorization: Bearer sk-secret-value"}}`)
	if strings.Contains(got.Message, "sk-secret-value") {
		t.Fatalf("message leaked the key: %q", got.Message)
	}
}

func TestProbeMarksReadyAndReadsModels(t *testing.T) {
	relay, server := newFakeRelay(t)
	client, store := newClient(t, server)

	health, err := client.Probe(context.Background(), "relay-1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !health.Ready {
		t.Fatalf("health = %+v", health)
	}
	if store.observed["relay-1"] != "ready" {
		t.Fatalf("observed = %v", store.observed)
	}

	relay.mu.Lock()
	relay.status = http.StatusUnauthorized
	relay.mu.Unlock()
	health, err = client.Probe(context.Background(), "relay-1")
	if err != nil {
		t.Fatalf("Probe must report problems through health, not error: %v", err)
	}
	if health.Ready || health.LastError == "" {
		t.Fatalf("health after a 401 = %+v", health)
	}
}

func TestAdapterDeclaresChatWithoutLogin(t *testing.T) {
	client := NewClient(newFakeStore(t, "relay-1", []byte(`{}`)))
	adapter := client.Adapter()
	if adapter.ID != ProviderID {
		t.Fatalf("adapter id = %q", adapter.ID)
	}
	if adapter.Login != nil {
		t.Fatal("a relay provider must not expose a browser login")
	}
	if !adapter.Supports("chat") || !adapter.Supports("models") {
		t.Fatal("chat and models must be supported")
	}
	if adapter.Supports("login") {
		t.Fatal("login must not be advertised")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// The console's single paste box hands the whole string over as api_key; a
// relay needs a base URL too, so the compact "url|key" form has to work there.
func TestDecodeCompactBaseAndKey(t *testing.T) {
	for _, raw := range []string{
		`{"api_key":"https://relay.example.com/v1|sk-abc"}`,
		`{"api_key":" https://relay.example.com/v1 sk-abc "}`,
	} {
		credential, err := DecodeCredential([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if credential.APIKey != "sk-abc" {
			t.Fatalf("%s → api_key %q", raw, credential.APIKey)
		}
		if base, err := credential.ChatBase(); err != nil || base != "https://relay.example.com/v1" {
			t.Fatalf("%s → base %q err=%v", raw, base, err)
		}
	}
	// A key that only contains a pipe must not be split.
	credential, err := DecodeCredential([]byte(`{"base_url":"https://relay.example.com/v1","api_key":"sk-a|b"}`))
	if err != nil || credential.APIKey != "sk-a|b" {
		t.Fatalf("credential = %+v err=%v", credential, err)
	}
}
