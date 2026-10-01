package zcode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// memStore is the minimal in-memory Store the chat tests need. It stores one
// credential payload per account id and reports a fixed region.
type memStore struct {
	items   map[string][]byte
	region  string
	account accounts.Account
}

func (s *memStore) Get(ctx context.Context, id string) (accounts.Account, error) {
	acct := s.account
	if acct.ID == "" {
		acct = accounts.Account{ID: id, Provider: "zcode"}
	}
	if acct.ProviderRegion == "" {
		acct.ProviderRegion = s.region
	}
	if acct.ProviderRegion == "" {
		acct.ProviderRegion = RegionBigModel
	}
	return acct, nil
}

func (s *memStore) LoadCredentialPayload(ctx context.Context, accountID string) (string, []byte, error) {
	payload, ok := s.items[accountID]
	if !ok {
		return "", nil, accounts.ErrAccountNotFound
	}
	return CredentialFormat, payload, nil
}

func (s *memStore) SaveCredentialPayload(ctx context.Context, accountID, format string, payload []byte) error {
	if s.items == nil {
		s.items = map[string][]byte{}
	}
	s.items[accountID] = payload
	return nil
}

func (s *memStore) Observe(ctx context.Context, id, remoteUID, status, lastError, lastKind string) error {
	return nil
}

// rewriteTransport redirects every request to the test server. The chat URL
// comes from the region descriptor; the test swaps scheme+host.
type rewriteTransport struct {
	server string
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.server, "http://")
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// anthropicStream is a canned Anthropic Messages SSE body with text, one
// tool_use call (whose arguments arrive in two input_json_delta frames), and
// a thinking block. Asserted chunk-for-chunk below.
const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"GLM-5.3","usage":{"input_tokens":12,"cache_read_input_tokens":4}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_time"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"tz\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"UTC\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// openAIChunk captures the fields the tests assert on.
type openAIChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int     `json:"index"`
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		CacheReadTokens  int `json:"cache_read_tokens"`
	} `json:"usage"`
}

// readOpenAIChunks parses an OpenAI SSE body into a chunk sequence plus the
// trailing [DONE] marker presence.
func readOpenAIChunks(t *testing.T, body string) ([]openAIChunk, bool) {
	t.Helper()
	var chunks []openAIChunk
	sawDone := false
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		if payload == "" {
			continue
		}
		var c openAIChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("unmarshal chunk %q: %v", payload, err)
		}
		chunks = append(chunks, c)
	}
	return chunks, sawDone
}

func TestTranslateAnthropicStream_TextToolUseFinish(t *testing.T) {
	var buf strings.Builder
	if err := translateAnthropicStream(strings.NewReader(anthropicStream), &buf); err != nil {
		t.Fatalf("translateAnthropicStream: %v", err)
	}
	chunks, sawDone := readOpenAIChunks(t, buf.String())
	if !sawDone {
		t.Fatalf("missing [DONE]; body=%s", buf.String())
	}
	if len(chunks) == 0 {
		t.Fatalf("no chunks emitted; body=%s", buf.String())
	}

	var (
		sawRole      bool
		sawReasoning string
		sawText      string
		toolIDSeen   string
		toolNameSeen string
		toolArgs     string
		finalFinish  string
		finalUsageOK bool
		chunkIDs     = map[string]bool{}
	)
	for i, c := range chunks {
		if c.Object != "chat.completion.chunk" {
			t.Errorf("chunk[%d].object=%q", i, c.Object)
		}
		if c.ID == "" {
			t.Errorf("chunk[%d].id empty", i)
		}
		chunkIDs[c.ID] = true
		if c.Model != "GLM-5.3" {
			t.Errorf("chunk[%d].model=%q want GLM-5.3", i, c.Model)
		}
		if len(c.Choices) == 0 {
			t.Fatalf("chunk[%d] missing choices", i)
		}
		ch := c.Choices[0]
		if ch.Delta.Role == "assistant" {
			sawRole = true
		}
		sawReasoning += ch.Delta.ReasoningContent
		sawText += ch.Delta.Content
		for _, call := range ch.Delta.ToolCalls {
			if call.ID != "" {
				toolIDSeen = call.ID
			}
			if call.Function.Name != "" {
				toolNameSeen = call.Function.Name
			}
			toolArgs += call.Function.Arguments
		}
		if ch.FinishReason != nil {
			finalFinish = *ch.FinishReason
		}
		if c.Usage != nil {
			finalUsageOK = c.Usage.PromptTokens == 12 && c.Usage.CompletionTokens == 7 && c.Usage.TotalTokens == 19 && c.Usage.CacheReadTokens == 4
		}
	}
	if !sawRole {
		t.Errorf("expected a role=assistant delta")
	}
	if sawReasoning != "plan" {
		t.Errorf("reasoning=%q want %q", sawReasoning, "plan")
	}
	if sawText != "Hello" {
		t.Errorf("text=%q want %q", sawText, "Hello")
	}
	if toolIDSeen != "toolu_1" {
		t.Errorf("tool id=%q want toolu_1", toolIDSeen)
	}
	if toolNameSeen != "get_time" {
		t.Errorf("tool name=%q want get_time", toolNameSeen)
	}
	if toolArgs != `{"tz":"UTC"}` {
		t.Errorf("tool args=%q", toolArgs)
	}
	if finalFinish != "tool_calls" {
		t.Errorf("finish=%q want tool_calls", finalFinish)
	}
	if !finalUsageOK {
		t.Errorf("usage missing or wrong on final chunk")
	}
	if len(chunkIDs) != 1 {
		t.Errorf("expected a single chunk id, got %v", chunkIDs)
	}
}

func TestAggregate_TextAndToolCalls(t *testing.T) {
	agg, err := Aggregate(strings.NewReader(anthropicStream))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if agg["object"] != "chat.completion" {
		t.Errorf("object=%v", agg["object"])
	}
	if agg["model"] != "GLM-5.3" {
		t.Errorf("model=%v", agg["model"])
	}
	choices, _ := agg["choices"].([]map[string]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%v", agg["choices"])
	}
	if choices[0]["finish_reason"] != "tool_calls" {
		t.Errorf("finish=%v", choices[0]["finish_reason"])
	}
	message, _ := choices[0]["message"].(map[string]any)
	if message["content"] != "Hello" {
		t.Errorf("content=%v", message["content"])
	}
	if message["reasoning_content"] != "plan" {
		t.Errorf("reasoning=%v", message["reasoning_content"])
	}
	toolCalls, _ := message["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls=%v", message["tool_calls"])
	}
	call, _ := toolCalls[0].(map[string]any)
	if call["id"] != "toolu_1" || call["type"] != "function" {
		t.Errorf("call=%v", call)
	}
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_time" {
		t.Errorf("fn.name=%v", fn["name"])
	}
	if fn["arguments"] != `{"tz":"UTC"}` {
		t.Errorf("fn.arguments=%v", fn["arguments"])
	}
	usage, _ := agg["usage"].(map[string]any)
	if usage["prompt_tokens"] != 12 || usage["completion_tokens"] != 7 || usage["total_tokens"] != 19 {
		t.Errorf("usage=%v", usage)
	}
	if usage["cache_read_tokens"] != 4 {
		t.Errorf("cache_read_tokens=%v", usage["cache_read_tokens"])
	}
}

func TestChatStream_HeadersAndBody(t *testing.T) {
	var sawAuth, sawAPIKey, sawVersion, sawUA string
	var sawBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the chat POST carries the auth header assertions; the catalog
		// prefetch is a GET to /api/v1/client/configs and returns an empty
		// catalogue so chat falls through to the static list.
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		sawAuth = r.Header.Get("Authorization")
		sawAPIKey = r.Header.Get("x-api-key")
		sawVersion = r.Header.Get("anthropic-version")
		sawUA = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&sawBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if r.URL.Path != "/api/anthropic/v1/messages" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicStream))
	}))
	defer server.Close()

	store := &memStore{
		items: map[string][]byte{
			"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"oauth","provider":"zai","zcode_jwt_token":"jwt-abc"}`),
		},
	}
	client := NewClient(store)
	client.http = server.Client()
	client.http.Transport = rewriteTransport{server: server.URL, base: client.http.Transport}

	resp, _, err := client.ChatStream(context.Background(), "acc1", translate.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		buf.WriteString(scanner.Text())
		buf.WriteByte('\n')
	}
	if sawAPIKey != "jwt-abc" {
		t.Errorf("x-api-key=%q", sawAPIKey)
	}
	if sawAuth != "Bearer jwt-abc" {
		t.Errorf("Authorization=%q", sawAuth)
	}
	if sawVersion != "2023-06-01" {
		t.Errorf("anthropic-version=%q", sawVersion)
	}
	if !strings.HasPrefix(sawUA, "ZCode/") {
		t.Errorf("User-Agent=%q", sawUA)
	}
	if sawBody["stream"] != true {
		t.Errorf("body.stream=%v", sawBody["stream"])
	}
	if sawBody["model"] != "GLM-5.3" {
		t.Errorf("body.model=%v", sawBody["model"])
	}
	chunks, sawDone := readOpenAIChunks(t, buf.String())
	if !sawDone || len(chunks) == 0 {
		t.Fatalf("body not translated: %s", buf.String())
	}
}

func TestChatStream_ClassifiesUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"token expired or incorrect","type":"401"}}`))
	}))
	defer server.Close()
	store := &memStore{
		items: map[string][]byte{
			"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"zai","api_key":"abc.def"}`),
		},
	}
	client := NewClient(store)
	client.http = server.Client()
	client.http.Transport = rewriteTransport{server: server.URL, base: client.http.Transport}
	_, _, err := client.ChatStream(context.Background(), "acc1", translate.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	var perr *providers.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type %T", err)
	}
	if perr.Kind != accounts.KindAuth {
		t.Errorf("Kind=%q want %q", perr.Kind, accounts.KindAuth)
	}
	if perr.Status != http.StatusUnauthorized {
		t.Errorf("Status=%d", perr.Status)
	}
}

func TestChatNonStream_CollectsOutcome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicStream))
	}))
	defer server.Close()
	store := &memStore{
		items: map[string][]byte{
			"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"zai","api_key":"abc.def"}`),
		},
	}
	client := NewClient(store)
	client.http = server.Client()
	client.http.Transport = rewriteTransport{server: server.URL, base: client.http.Transport}
	out, err := client.ChatNonStream(context.Background(), "acc1", translate.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatNonStream: %v", err)
	}
	if out.Content != "Hello" {
		t.Errorf("Content=%q", out.Content)
	}
	if out.Reasoning != "plan" {
		t.Errorf("Reasoning=%q", out.Reasoning)
	}
	if out.FinishReason != "tool_calls" {
		t.Errorf("FinishReason=%q", out.FinishReason)
	}
	if out.PromptTokens != 12 || out.CompletionTokens != 7 {
		t.Errorf("tokens=%d/%d", out.PromptTokens, out.CompletionTokens)
	}
	if out.CacheReadTokens == nil || *out.CacheReadTokens != 4 {
		t.Errorf("CacheReadTokens=%v", out.CacheReadTokens)
	}
	if len(out.ToolCalls) == 0 {
		t.Errorf("ToolCalls empty")
	}
}

// Anthropic Messages requires max_tokens; the body must always carry a
// positive value, sourced from the client when present, else the catalog
// MaxOutput cap, else the safe default. Reasoning uses output_config.effort
// only — the upstream plan gateway has no `thinking` field.
func TestAnthropicBody_MaxTokensAlwaysPresent(t *testing.T) {
	req := translate.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	}
	// No client max_tokens, no catalog cap → default.
	body := anthropicBody(req, "", 0)
	mt, ok := body["max_tokens"].(int)
	if !ok || mt <= 0 {
		t.Fatalf("max_tokens missing or non-positive: %v", body["max_tokens"])
	}
	if mt != defaultMaxTokens {
		t.Errorf("max_tokens=%d, want default %d", mt, defaultMaxTokens)
	}
	// Catalog cap wins when the client omitted a value.
	body = anthropicBody(req, "", 128_000)
	if got := body["max_tokens"]; got != 128_000 {
		t.Errorf("max_tokens=%v, want catalog cap 128000", got)
	}
	// Client value passes through unchanged (no cap at catalog max).
	withClient := translate.ChatRequest{
		Model:           "GLM-5.3",
		Messages:        []translate.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens:       json.RawMessage("4096"),
		Stop:            nil,
		ReasoningEffort: nil,
	}
	body = anthropicBody(withClient, "", 128_000)
	if got := body["max_tokens"]; got != 4096 {
		t.Errorf("max_tokens=%v, want client value 4096", got)
	}
	// max_completion_tokens is honored when max_tokens is absent.
	withCompletion := translate.ChatRequest{
		Model:               "GLM-5.3",
		Messages:            []translate.ChatMessage{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: json.RawMessage("2048"),
	}
	body = anthropicBody(withCompletion, "", 128_000)
	if got := body["max_tokens"]; got != 2048 {
		t.Errorf("max_tokens=%v, want 2048 from max_completion_tokens", got)
	}
	// Non-positive client value falls through to catalog/default.
	bad := translate.ChatRequest{
		Model:     "GLM-5.3",
		Messages:  []translate.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens: json.RawMessage("0"),
	}
	body = anthropicBody(bad, "", 128_000)
	if got := body["max_tokens"]; got != 128_000 {
		t.Errorf("max_tokens=%v, want catalog cap 128000 for zero client value", got)
	}
}

func TestAnthropicBody_ReasoningOmitsThinking(t *testing.T) {
	req := translate.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	}
	body := anthropicBody(req, "high", 128_000)
	if _, hasThinking := body["thinking"]; hasThinking {
		t.Errorf("thinking field must not be sent; got %v", body["thinking"])
	}
	effort, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config missing: %v", body)
	}
	if got := effort["effort"]; got != "high" {
		t.Errorf("output_config.effort=%v, want high", got)
	}
	// "none" and "" must not emit output_config either.
	for _, level := range []string{"", "none"} {
		body = anthropicBody(req, level, 128_000)
		if _, present := body["output_config"]; present {
			t.Errorf("output_config emitted for level %q", level)
		}
	}
}
