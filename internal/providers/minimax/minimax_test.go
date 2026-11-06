package minimax

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// ---- fakes ----------------------------------------------------------------

type fakeStore struct {
	mu       sync.Mutex
	payload  []byte
	account  accounts.Account
	observed string
	saves    int
}

func (s *fakeStore) Get(context.Context, string) (accounts.Account, error) { return s.account, nil }

func (s *fakeStore) LoadCredentialPayload(context.Context, string) (string, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return CredentialFormat, append([]byte(nil), s.payload...), nil
}

func (s *fakeStore) SaveCredentialPayload(_ context.Context, _ string, _ string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payload = append([]byte(nil), payload...)
	s.saves++
	return nil
}

func (s *fakeStore) Observe(_ context.Context, _, _, status, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = status
	return nil
}

func (s *fakeStore) saved(t *testing.T) Credential {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, err := DecodeCredential(s.payload)
	if err != nil {
		t.Fatalf("decode saved credential: %v", err)
	}
	return cred
}

func storeWith(t *testing.T, cred Credential) *fakeStore {
	t.Helper()
	payload, err := EncodeCredential(cred)
	if err != nil {
		t.Fatalf("encode credential: %v", err)
	}
	return &fakeStore{
		payload: payload,
		account: accounts.Account{ID: "acc_minimax", Provider: ProviderID, ProviderRegion: "cn"},
	}
}

// anthropicSSE is one MiniMax-shaped Messages stream: a text block, a tool_use
// block, and the closing frames. The gateway emits exactly this.
const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"MiniMax-M3","usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"get_time","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Beijing\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// ---- credential -----------------------------------------------------------

func TestDecodeCredentialShapes(t *testing.T) {
	now := time.Now().Unix()

	t.Run("canonical", func(t *testing.T) {
		cred, err := DecodeCredential([]byte(`{"format":"minimax-oauth-v1","access_token":"at","refresh_token":"rt","expires_at":` + itoa(now+3600) + `,"region":"global"}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cred.AccessToken != "at" || cred.RefreshToken != "rt" || cred.Region != "global" {
			t.Fatalf("unexpected credential: %+v", cred)
		}
		if cred.ExpiresAt != now+3600 {
			t.Fatalf("expires_at = %d, want %d", cred.ExpiresAt, now+3600)
		}
	})

	t.Run("bridge tokens.json in milliseconds", func(t *testing.T) {
		cred, err := DecodeCredential([]byte(`{"cn":{"accessToken":"at","refreshToken":"rt","expiresAt":` + itoa((now+3600)*1000) + `}}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cred.AccessToken != "at" || cred.RefreshToken != "rt" {
			t.Fatalf("unexpected credential: %+v", cred)
		}
		if cred.Region != "cn" {
			t.Fatalf("region = %q, want cn", cred.Region)
		}
		if cred.ExpiresAt != now+3600 {
			t.Fatalf("expires_at = %d, want %d", cred.ExpiresAt, now+3600)
		}
	})

	t.Run("server expires_in is a duration", func(t *testing.T) {
		cred, err := DecodeCredential([]byte(`{"access_token":"at","expires_in":43200}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		// expiring in twelve hours, not in 1970
		if cred.ExpiresAt < now+43000 || cred.ExpiresAt > now+43400 {
			t.Fatalf("expires_at = %d, want about %d", cred.ExpiresAt, now+43200)
		}
		if cred.Region != "cn" {
			t.Fatalf("default region = %q, want cn", cred.Region)
		}
	})

	t.Run("console paste box wraps the token in api_key", func(t *testing.T) {
		cred, err := DecodeCredential([]byte(`{"api_key":"at-box"}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cred.AccessToken != "at-box" {
			t.Fatalf("access token = %q, want at-box", cred.AccessToken)
		}
	})

	t.Run("bare token paste", func(t *testing.T) {
		cred, err := DecodeCredential([]byte("Bearer abc.def"))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cred.AccessToken != "abc.def" {
			t.Fatalf("access token = %q", cred.AccessToken)
		}
		if cred.CanRefresh() {
			t.Fatal("a bare token has nothing to refresh with")
		}
	})
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		cred Credential
		want bool
	}{
		{"fresh", Credential{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(6 * time.Hour).Unix()}, false},
		{"close to the deadline", Credential{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(10 * time.Minute).Unix()}, true},
		{"expired", Credential{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(-time.Hour).Unix()}, true},
		{"no token but refreshable", Credential{RefreshToken: "rt"}, true},
		{"no deadline", Credential{AccessToken: "at", RefreshToken: "rt"}, false},
		{"nothing to refresh with", Credential{AccessToken: "at", ExpiresAt: now.Add(-time.Hour).Unix()}, true},
	}
	for _, test := range cases {
		if got := test.cred.NeedsRefresh(now); got != test.want {
			t.Errorf("%s: NeedsRefresh = %v, want %v", test.name, got, test.want)
		}
	}
}

// ---- catalog --------------------------------------------------------------

func TestParseCatalogReadsTheMapShape(t *testing.T) {
	body := `{"version":"1.0","ttlSeconds":300,"providers":[{"providerId":"minimax","config":{"npm":"@ai-sdk/anthropic","models":{
		"MiniMax-M3":{"name":"M3","tool_call":true,"reasoning":true,"limit":{"context":512000,"output":128000},"modalities":{"input":["text","image","video"]}},
		"MiniMax-M9":{"name":"","tool_call":false,"limit":{"context":1000,"output":100}}}}}]}`
	models, err := parseCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	// sorted by id
	if models[0].PublicModel != "MiniMax-M3" || models[1].PublicModel != "MiniMax-M9" {
		t.Fatalf("unexpected order: %s, %s", models[0].PublicModel, models[1].PublicModel)
	}
	m3 := models[0]
	if !m3.Capabilities.Tools || !m3.Capabilities.Images || !m3.Capabilities.Reasoning {
		t.Fatalf("M3 capabilities = %+v", m3.Capabilities)
	}
	if m3.Capabilities.ContextWindow != 512000 || m3.Capabilities.MaxOutput != 128000 {
		t.Fatalf("M3 limits = %+v", m3.Capabilities)
	}
	// An unknown model with no friendly name still gets a usable label.
	if models[1].DisplayName != "MiniMax-M9" {
		t.Fatalf("display name = %q", models[1].DisplayName)
	}
}

func TestModelsFallsBackToStaticWhenTheGatewayFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	store := storeWith(t, Credential{AccessToken: "at", RefreshToken: "rt", Region: "cn", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	client := NewClient(store)
	client.siteResolver = fixedSite(srv.URL)

	models, err := client.Models(context.Background(), "acc_minimax")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != len(staticCatalog) {
		t.Fatalf("got %d models, want the %d-entry fallback", len(models), len(staticCatalog))
	}
}

// ---- chat -----------------------------------------------------------------

func TestChatStreamRelaysTranslatedChunks(t *testing.T) {
	var seenAuth, seenAgent, seenVersion atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mavis/api/v1/models" {
			// The capability lookup asks for the catalog first; answer with an
			// empty one so the static fallback supplies the limits.
			io.WriteString(w, `{"providers":[]}`)
			return
		}
		seenAuth.Store(r.Header.Get("Authorization"))
		seenAgent.Store(r.Header.Get("x-mavis-agent-id"))
		seenVersion.Store(r.Header.Get("anthropic-version"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("upstream stream = %v, want true", body["stream"])
		}
		if body["model"] != "MiniMax-M3" {
			t.Errorf("upstream model = %v", body["model"])
		}
		if body["system"] != "be terse" {
			t.Errorf("upstream system = %v", body["system"])
		}
		if _, ok := body["tools"]; !ok {
			t.Error("tools were dropped")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer srv.Close()

	store := storeWith(t, Credential{AccessToken: "at", RefreshToken: "rt", Region: "cn", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	client := NewClient(store)
	client.siteResolver = fixedSite(srv.URL)

	req := translate.ChatRequest{
		Model:    "MiniMax-M3",
		Messages: []translate.ChatMessage{{Role: "system", Content: "be terse"}, {Role: "user", Content: "hi"}},
		Tools:    json.RawMessage(`[{"type":"function","function":{"name":"get_time","description":"time","parameters":{"type":"object","properties":{}}}}]`),
	}
	resp, _, err := client.ChatStream(context.Background(), "acc_minimax", req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if !strings.Contains(text, `"content":"Hel"`) || !strings.Contains(text, `"content":"lo"`) {
		t.Fatalf("text deltas missing from the translated stream:\n%s", text)
	}
	if !strings.Contains(text, `"tool_calls"`) || !strings.Contains(text, `get_time`) {
		t.Fatalf("tool call missing from the translated stream:\n%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason was not reconciled to tool_calls:\n%s", text)
	}
	if seenAuth.Load() != "Bearer at" {
		t.Errorf("Authorization = %v", seenAuth.Load())
	}
	if seenAgent.Load() != AgentID {
		t.Errorf("x-mavis-agent-id = %v", seenAgent.Load())
	}
	if seenVersion.Load() != AnthropicVersion {
		t.Errorf("anthropic-version = %v", seenVersion.Load())
	}
}

func TestChatNonStreamAssemblesTheStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mavis/api/v1/models" {
			io.WriteString(w, `{"providers":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer srv.Close()
	store := storeWith(t, Credential{AccessToken: "at", RefreshToken: "rt", Region: "cn", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	client := NewClient(store)
	client.siteResolver = fixedSite(srv.URL)

	outcome, err := client.ChatNonStream(context.Background(), "acc_minimax", translate.ChatRequest{
		Model:    "MiniMax-M3",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatNonStream: %v", err)
	}
	if outcome.Content != "Hello" {
		t.Fatalf("content = %q, want Hello", outcome.Content)
	}
	if outcome.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", outcome.FinishReason)
	}
	if outcome.PromptTokens != 12 || outcome.CompletionTokens != 7 {
		t.Fatalf("usage = %d/%d", outcome.PromptTokens, outcome.CompletionTokens)
	}
	if !strings.Contains(string(outcome.ToolCalls), "get_time") {
		t.Fatalf("tool calls = %s", outcome.ToolCalls)
	}
}

// TestChatRefreshesOnceOn401 is the single-use refresh contract: an expired
// access token costs exactly one refresh, the retry carries the new token, and
// the rotated pair is persisted.
func TestChatRefreshesOnceOn401(t *testing.T) {
	var chatCalls, refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case TokenPath:
			refreshCalls.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-old" {
				t.Errorf("refresh form = %v", r.Form)
			}
			io.WriteString(w, `{"access_token":"at-new","refresh_token":"rt-new","expires_in":43200}`)
		case "/mavis/api/v1/models":
			io.WriteString(w, `{"providers":[]}`)
		case MessagesPath:
			chatCalls.Add(1)
			if r.Header.Get("Authorization") == "Bearer at-old" {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, anthropicSSE)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := storeWith(t, Credential{AccessToken: "at-old", RefreshToken: "rt-old", Region: "cn", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	client := NewClient(store)
	client.siteResolver = fixedSite(srv.URL)

	if _, err := client.ChatNonStream(context.Background(), "acc_minimax", translate.ChatRequest{
		Model: "MiniMax-M3", Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatNonStream: %v", err)
	}
	if chatCalls.Load() != 2 {
		t.Fatalf("chat calls = %d, want 2 (one refused, one retried)", chatCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshCalls.Load())
	}
	saved := store.saved(t)
	if saved.AccessToken != "at-new" || saved.RefreshToken != "rt-new" {
		t.Fatalf("saved credential = %+v", saved)
	}
	if saved.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("saved deadline not in the future: %d", saved.ExpiresAt)
	}
}

func TestConcurrent401sShareOneRefresh(t *testing.T) {
	var refreshCalls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case TokenPath:
			refreshCalls.Add(1)
			<-release
			io.WriteString(w, `{"access_token":"at-new","refresh_token":"rt-new","expires_in":43200}`)
		case "/mavis/api/v1/models":
			io.WriteString(w, `{"providers":[]}`)
		case MessagesPath:
			if r.Header.Get("Authorization") == "Bearer at-old" {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"expired"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, anthropicSSE)
		}
	}))
	defer srv.Close()

	store := storeWith(t, Credential{AccessToken: "at-old", RefreshToken: "rt-old", Region: "cn", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	client := NewClient(store)
	client.siteResolver = fixedSite(srv.URL)

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, errs[index] = client.ChatNonStream(context.Background(), "acc_minimax", translate.ChatRequest{
				Model: "MiniMax-M3", Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
			})
		}(i)
	}
	// Let all three hit the 401 before the refresh answers, which is exactly
	// the race that would spend three single-use refresh tokens.
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1 — a spent refresh token can kill the slot", refreshCalls.Load())
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"dead token", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`, accounts.KindAuth},
		{"spent refresh", 400, `{"error":"invalid_grant","error_description":"refresh token already used"}`, accounts.KindAuth},
		{"rate limit", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, accounts.KindRateLimit},
		{"no quota", 200, `{"base_resp":{"status_code":1008,"status_msg":"积分已用完"}}`, accounts.KindQuota},
		{"bad request", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens too large"}}`, accounts.KindInvalidRequest},
		{"missing model", 404, `{"type":"error","error":{"type":"not_found_error","message":"no such model"}}`, accounts.KindModelNotAvailable},
		{"overloaded", 529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, accounts.KindUnavailable},
		{"server error", 502, `bad gateway`, accounts.KindUnavailable},
	}
	for _, test := range cases {
		if got := Classify(test.status, test.body); got.Kind != test.want {
			t.Errorf("%s: kind = %q, want %q", test.name, got.Kind, test.want)
		}
	}
}

// ---- login ----------------------------------------------------------------

func TestDeviceFlowLogin(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case DeviceCodePath:
			_ = r.ParseForm()
			if r.Form.Get("code_challenge_method") != "S256" || r.Form.Get("client_id") != PublicClientID {
				t.Errorf("device form = %v", r.Form)
			}
			io.WriteString(w, `{"user_code":"ABC-123","device_code":"dev-1","verification_uri_complete":"https://example.test/verify?code=ABC-123","expires_in":600,"interval":1}`)
		case TokenPath:
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != DeviceGrant || r.Form.Get("device_code") != "dev-1" || r.Form.Get("code_verifier") == "" {
				t.Errorf("poll form = %v", r.Form)
			}
			if polls.Add(1) == 1 {
				io.WriteString(w, `{"status":"pending"}`)
				return
			}
			io.WriteString(w, `{"access_token":"at-login","refresh_token":"rt-login","expires_in":43200,"token_type":"Bearer"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &fakeStore{account: accounts.Account{ID: "acc_minimax", Provider: ProviderID, ProviderRegion: "cn"}}
	client := NewClient(store)
	client.siteResolver = fixedSite(srv.URL)

	session, err := client.StartLogin(context.Background(), "acc_minimax")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if !strings.Contains(session.AuthURL, "example.test") || session.State != "ABC-123" {
		t.Fatalf("session = %+v", session)
	}
	done, message, err := client.PollLogin(context.Background(), "acc_minimax")
	if err != nil || done {
		t.Fatalf("first poll: done=%v err=%v", done, err)
	}
	if !strings.Contains(message, "ABC-123") {
		t.Fatalf("waiting message = %q", message)
	}
	// The poll interval is one second; wait it out rather than sleeping inside.
	time.Sleep(1100 * time.Millisecond)
	done, _, err = client.PollLogin(context.Background(), "acc_minimax")
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if !done {
		t.Fatal("login did not complete")
	}
	saved := store.saved(t)
	if saved.AccessToken != "at-login" || saved.RefreshToken != "rt-login" || saved.Region != "cn" {
		t.Fatalf("saved credential = %+v", saved)
	}
	if store.observed != "ready" {
		t.Fatalf("observed status = %q", store.observed)
	}
}

// fixedSite points every region at one test server.
func fixedSite(url string) func(string) Site {
	return func(region string) Site {
		site := siteFor(region)
		site.AccountHost = url
		site.AgentHost = url
		return site
	}
}

func itoa(value int64) string {
	return strings.TrimSpace(string(mustJSON(value)))
}

func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
