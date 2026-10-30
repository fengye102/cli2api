package autoclaw

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
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// memoryStore is a minimal Store for adapter tests.
type memoryStore struct {
	mu       sync.Mutex
	payload  []byte
	account  accounts.Account
	saved    []byte
	observed []string
}

func (s *memoryStore) Get(context.Context, string) (accounts.Account, error) {
	return s.account, nil
}

func (s *memoryStore) LoadCredentialPayload(context.Context, string) (string, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return CredentialFormat, append([]byte(nil), s.payload...), nil
}

func (s *memoryStore) SaveCredentialPayload(_ context.Context, _ string, _ string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payload = append([]byte(nil), payload...)
	s.saved = append([]byte(nil), payload...)
	return nil
}

func (s *memoryStore) Observe(_ context.Context, _ string, _ string, status string, _ string, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = append(s.observed, status)
	return nil
}

func testCredential() Credential {
	return Credential{
		Format:       CredentialFormat,
		AccessToken:  "ACCESS-TOKEN",
		RefreshToken: "REFRESH-TOKEN",
		ExpiresAt:    time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		UserID:       "user-1",
	}
}

func newTestClient(t *testing.T, server *httptest.Server) (*Client, *memoryStore) {
	t.Helper()
	payload, err := EncodeCredential(testCredential())
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{
		payload: payload,
		account: accounts.Account{ID: "acct-1", Provider: ProviderID, ProviderRegion: "cn-north-4"},
	}
	client := NewClient(store, RegionCN)
	client.upstreamBase = server.URL + "/autoclaw-proxy/proxy/autoclaw"
	client.userapiBase = server.URL
	client.nowFn = func() time.Time { return time.Now() }
	return client, store
}

// TestPromptPrefixIsAddedToFirstSystemMessage pins the gate that turns an
// ordinary request into an empty 406: the system prompt must begin with the
// OpenClaw identity line and carry a "## Tooling" section.
func TestPromptPrefixIsAddedToFirstSystemMessage(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	normalizePrompt(body)

	messages := body["messages"].([]any)
	first := messages[0].(map[string]any)
	content := first["content"].(string)
	if !strings.HasPrefix(content, identityLine) {
		t.Fatalf("system prompt does not start with the identity line: %q", content)
	}
	if !strings.Contains(content, "## Tooling") {
		t.Fatalf("system prompt is missing the Tooling section: %q", content)
	}
	// The caller's own prompt must survive verbatim underneath.
	if !strings.Contains(content, "You are a helpful assistant.") {
		t.Fatalf("caller prompt was dropped: %q", content)
	}
	if len(messages) != 2 {
		t.Fatalf("message count changed: %d", len(messages))
	}
}

// TestPromptInsertedWhenNoSystemMessage covers a client that sends no system
// message at all, which the upstream rejects with an empty 406.
func TestPromptInsertedWhenNoSystemMessage(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	normalizePrompt(body)

	messages := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected an inserted system message, got %d messages", len(messages))
	}
	first := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first message is %v, want system", first["role"])
	}
	if !strings.HasPrefix(first["content"].(string), identityLine) {
		t.Fatalf("inserted prompt is wrong: %q", first["content"])
	}
	// The original user message must still be second and unmodified.
	second := messages[1].(map[string]any)
	if second["content"] != "hi" {
		t.Fatalf("user message was modified: %v", second["content"])
	}
}

// TestPromptIsIdempotent guards the degraded/retry path: prefixing twice would
// duplicate the prompt.
func TestPromptIsIdempotent(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": identityPrefix},
		},
	}
	normalizePrompt(body)
	first := body["messages"].([]any)[0].(map[string]any)
	if count := strings.Count(first["content"].(string), identityLine); count != 1 {
		t.Fatalf("identity line appears %d times, want 1", count)
	}
}

// TestForeignIdentitiesAreRewritten covers the second half of the same gate:
// a system prompt naming a foreign harness is rejected even with the prefix.
func TestForeignIdentitiesAreRewritten(t *testing.T) {
	cases := []string{
		"You are Codex, based on GPT-5. You are running as a coding agent.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.",
		"You are ZCode, an interactive coding agent",
		"You are an AI agent powered by DeepSeek Harness",
	}
	for _, input := range cases {
		body := map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": input},
			},
		}
		normalizePrompt(body)
		content := body["messages"].([]any)[0].(map[string]any)["content"].(string)
		for _, blocked := range []string{"You are Codex", "You are Claude Code", "You are ZCode", "DeepSeek Harness"} {
			if strings.Contains(strings.ToLower(content), strings.ToLower(blocked)) {
				t.Fatalf("blocked identity %q survived rewriting: %q", blocked, content)
			}
		}
	}
}

// TestForeignIdentityRewriteIsCaseInsensitive pins the measured behaviour: the
// gate matches case-insensitively, so the rewrite must too.
func TestForeignIdentityRewriteIsCaseInsensitive(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "YOU ARE CLAUDE CODE, official CLI."},
		},
	}
	normalizePrompt(body)
	content := body["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(strings.ToLower(content), "you are claude code") {
		t.Fatalf("case variant survived: %q", content)
	}
}

// TestUserMessagesAreNeverTouched: the gate only reads system prompts, so
// editing user content would corrupt data.
func TestUserMessagesAreNeverTouched(t *testing.T) {
	userText := "You are Codex, based on GPT-5."
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": userText},
		},
	}
	normalizePrompt(body)
	got := body["messages"].([]any)[1].(map[string]any)["content"].(string)
	if got != userText {
		t.Fatalf("user message was rewritten: %q", got)
	}
}

// TestMaxTokensSmallValueRaisedToFloor pins the second gate: small positive
// values get an empty 406, so they are normalized up.
func TestMaxTokensSmallValueRaisedToFloor(t *testing.T) {
	for _, small := range []any{float64(30), float64(1024), float64(8192)} {
		body := map[string]any{"max_tokens": small}
		normalizeMaxTokens(body)
		if body["max_tokens"] != maxTokensFloor {
			t.Fatalf("max_tokens %v not raised to %d: %v", small, maxTokensFloor, body["max_tokens"])
		}
	}
}

// TestMaxTokensOtherShapesLeftAlone: only the measured small-positive window is
// touched; everything else is the upstream's call.
func TestMaxTokensOtherShapesLeftAlone(t *testing.T) {
	body := map[string]any{"max_tokens": float64(10000)}
	normalizeMaxTokens(body)
	if body["max_tokens"] != float64(10000) {
		t.Fatalf("value at the floor was changed: %v", body["max_tokens"])
	}

	body = map[string]any{"max_tokens": float64(0)}
	normalizeMaxTokens(body)
	if body["max_tokens"] != float64(0) {
		t.Fatalf("zero was rewritten: %v", body["max_tokens"])
	}

	body = map[string]any{"max_tokens": "1024"}
	normalizeMaxTokens(body)
	if body["max_tokens"] != "1024" {
		t.Fatalf("string value was rewritten: %v", body["max_tokens"])
	}

	// Absent stays absent: the upstream accepts it and injecting a number
	// would be guessing at its ceiling.
	body = map[string]any{}
	normalizeMaxTokens(body)
	if _, present := body["max_tokens"]; present {
		t.Fatal("absent max_tokens was injected")
	}
}

// TestModelRouteSplitsHeaderAndBody pins the two-identifier rule: the header
// carries the prefixed route id, the body carries the stripped id.
func TestModelRouteSplitsHeaderAndBody(t *testing.T) {
	cases := map[string]struct{ route, body string }{
		"glm-5.3":           {"zaicoding_glm-5.3", "glm-5.3"},
		"GLM-5.3":           {"zaicoding_glm-5.3", "glm-5.3"},
		"glm-5.3-flash":     {"zai_glm-5.3-flash", "glm-5.3-flash"},
		"zaicoding_glm-5.3": {"zaicoding_glm-5.3", "glm-5.3"},
		"GLM-5.3-Flash":     {"zai_glm-5.3-flash", "glm-5.3-flash"},
	}
	for input, want := range cases {
		got := resolveModelRoute(RegionCN, input)
		if got.RouteModelID != want.route {
			t.Fatalf("%q route id = %q, want %q", input, got.RouteModelID, want.route)
		}
		if got.BodyModelID != want.body {
			t.Fatalf("%q body id = %q, want %q", input, got.BodyModelID, want.body)
		}
	}
}

// TestUnknownModelFallsBackToDefault keeps Anthropic clients sending claude-*
// working instead of erroring.
func TestUnknownModelFallsBackToDefault(t *testing.T) {
	got := resolveModelRoute(RegionCN, "claude-sonnet-4")
	if got.RouteModelID != defaultRoute {
		t.Fatalf("route = %q, want %q", got.RouteModelID, defaultRoute)
	}
	if got.BodyModelID != "auto" {
		t.Fatalf("body id = %q, want auto", got.BodyModelID)
	}
}

// TestRoutePrefixStripOrder: zaicoding_ must be stripped before zai_, or
// "zaicoding_glm-5.3" becomes "coding_glm-5.3".
func TestRoutePrefixStripOrder(t *testing.T) {
	if got := stripRoutePrefix("zaicoding_glm-5.3"); got != "glm-5.3" {
		t.Fatalf("strip = %q, want glm-5.3", got)
	}
	if got := stripRoutePrefix("zai_auto"); got != "auto" {
		t.Fatalf("strip = %q, want auto", got)
	}
}

// TestChatBodyCarriesAllThreeGates checks the assembled outbound body: model
// stripped, max_tokens raised, prompt prefixed.
func TestChatBodyCarriesAllThreeGates(t *testing.T) {
	req := translate.ChatRequest{Model: "glm-5.3", MaxTokens: json.RawMessage("1024")}
	route := resolveModelRoute(RegionCN, req.Model)
	raw, err := buildChatBody(req, route)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "glm-5.3" {
		t.Fatalf("body model = %v, want the stripped id", body["model"])
	}
	if body["max_tokens"] != json.Number("10000") && body["max_tokens"] != float64(maxTokensFloor) {
		t.Fatalf("max_tokens = %v, want %d", body["max_tokens"], maxTokensFloor)
	}
}

// TestSignHeadersUsesSecondTimestamp is the highest-value signature assertion:
// a millisecond timestamp fails the signature check silently (code 400002) and
// only surfaces as a fallback refresh.
func TestSignHeadersUsesSecondTimestamp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	headers := signedHeaders("", now)
	if headers["X-Auth-TimeStamp"] != "1700000000" {
		t.Fatalf("timestamp = %q, want seconds", headers["X-Auth-TimeStamp"])
	}
	if headers["X-Auth-Appid"] != authAppID {
		t.Fatalf("appid = %q", headers["X-Auth-Appid"])
	}
	// X-Harness-Type belongs on the userapi path only.
	if headers["X-Harness-Type"] != harnessType {
		t.Fatalf("userapi headers lost X-Harness-Type: %v", headers)
	}
	// The signature is md5(appId&ts&appKey), lowercase hex.
	if len(headers["X-Auth-Sign"]) != 32 {
		t.Fatalf("sign length = %d, want 32 hex chars", len(headers["X-Auth-Sign"]))
	}
}

// TestBrandHeadersOmitHarnessType pins the deliberate asymmetry: sending
// X-Harness-Type: zcode on the chat path yields 403 pay-view / 406.
func TestBrandHeadersOmitHarnessType(t *testing.T) {
	headers := brandHeaders()
	if _, present := headers["X-Harness-Type"]; present {
		t.Fatal("chat headers must not carry X-Harness-Type")
	}
	if headers["X-Product"] != clientProduct {
		t.Fatalf("X-Product = %q", headers["X-Product"])
	}
	// Linux must never be reported: the upstream 403s X-Tm: linux.
	if headers["X-Tm"] == "linux" {
		t.Fatal("X-Tm must not be linux")
	}
}

// TestUserAgentIsNotGoDefault: the upstream WAF rejects Go's default
// User-Agent with a 405 HTML page, so every request path must override it.
//
// A regression here is invisible in unit tests that only inspect headers, and
// in production it looks like a routing bug (HTML body, no upstream error),
// which is why the assertion is on both header maps.
func TestUserAgentIsNotGoDefault(t *testing.T) {
	for name, headers := range map[string]map[string]string{
		"chat":    brandHeaders(),
		"userapi": signedHeaders("token", time.Now()),
	} {
		ua := headers["User-Agent"]
		if ua == "" {
			t.Fatalf("%s headers carry no User-Agent; the WAF 405s Go's default", name)
		}
		if strings.Contains(ua, "Go-http-client") {
			t.Fatalf("%s headers still carry the Go default UA %q", name, ua)
		}
		if ua != clientUserAgent {
			t.Fatalf("%s User-Agent = %q, want %q", name, ua, clientUserAgent)
		}
	}
}

// TestCredentialDecodeAcceptsDesktopShape: the desktop auth.json uses camelCase,
// and the console's paste path hands a bare token.
func TestCredentialDecodeAcceptsDesktopShape(t *testing.T) {
	desktop := []byte(`{"token":"T","refreshToken":"R","deviceId":"D","userInfo":{"uid":"U"}}`)
	cred, err := DecodeCredential(desktop)
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "T" || cred.RefreshToken != "R" || cred.DeviceID != "D" {
		t.Fatalf("decoded wrong: %+v", cred)
	}

	bare := []byte(`"BARE-TOKEN"`)
	cred, err = DecodeCredential(bare)
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "BARE-TOKEN" {
		t.Fatalf("bare token = %q", cred.AccessToken)
	}
}

// TestCredentialWithoutRefreshIsNotRefreshable stops maintenance from retrying
// a token-only credential forever.
func TestCredentialWithoutRefreshIsNotRefreshable(t *testing.T) {
	cred := Credential{AccessToken: "T"}
	if cred.CanRefresh() {
		t.Fatal("a token-only credential must not claim refreshability")
	}
	if !cred.Ready() {
		t.Fatal("a token-only credential is still usable")
	}
}

// TestNeedsRefreshWindow: only tokens inside the window trigger a refresh, and
// an unknown expiry defers to the upstream.
func TestNeedsRefreshWindow(t *testing.T) {
	now := time.Now()
	near := Credential{AccessToken: "T", ExpiresAt: now.Add(2 * time.Minute).Format(time.RFC3339)}
	if !near.NeedsRefresh(now) {
		t.Fatal("a token expiring in 2 minutes must need a refresh")
	}
	far := Credential{AccessToken: "T", ExpiresAt: now.Add(2 * time.Hour).Format(time.RFC3339)}
	if far.NeedsRefresh(now) {
		t.Fatal("a token expiring in 2 hours must not need a refresh")
	}
	unknown := Credential{AccessToken: "T"}
	if unknown.NeedsRefresh(now) {
		t.Fatal("an unknown expiry must not trigger a refresh")
	}
}

// TestRegionMapping pins the two-provider model.
func TestRegionMapping(t *testing.T) {
	if RegionCN.ProviderID() != ProviderID {
		t.Fatalf("CN provider id = %q", RegionCN.ProviderID())
	}
	if RegionIntl.ProviderID() != IntlProviderID {
		t.Fatalf("Intl provider id = %q", RegionIntl.ProviderID())
	}
	// The chat paths genuinely differ; getting this wrong is a 404 that looks
	// like an auth failure.
	if RegionCN.chatPath() != "/chat/completions" {
		t.Fatalf("CN chat path = %q", RegionCN.chatPath())
	}
	if RegionIntl.chatPath() != "/v1/chat/completions" {
		t.Fatalf("Intl chat path = %q", RegionIntl.chatPath())
	}
	if _, ok := ResolveRegion(ProviderID); !ok {
		t.Fatal("autoclaw must resolve")
	}
	if _, ok := ResolveRegion(IntlProviderID); !ok {
		t.Fatal("autoclaw-intl must resolve")
	}
	if _, ok := ResolveRegion("raccoon"); ok {
		t.Fatal("raccoon must not resolve as autoclaw")
	}
}

// TestChatSendsRouteHeaderAndStrippedBody is the end-to-end shape check against
// a stub upstream: the header carries the route id, the body the stripped id,
// and the auth header is X-Authorization.
func TestChatSendsRouteHeaderAndStrippedBody(t *testing.T) {
	var seenHeaders http.Header
	var seenBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		_ = decoder.Decode(&seenBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client, _ := newTestClient(t, server)
	req := translate.ChatRequest{
		Model: "glm-5.3",
		Messages: []translate.ChatMessage{
			{Role: "system", Content: "You are Claude Code, Anthropic's official CLI."},
			{Role: "user", Content: "hi"},
		},
		MaxTokens: json.RawMessage("512"),
	}
	resp, _, err := client.ChatStream(context.Background(), "acct-1", req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := seenHeaders.Get("X-Request-Model"); got != "zaicoding_glm-5.3" {
		t.Fatalf("X-Request-Model = %q, want zaicoding_glm-5.3", got)
	}
	if got := seenHeaders.Get("X-Authorization"); !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("X-Authorization = %q", got)
	}
	if got := seenHeaders.Get("X-Harness-Type"); got != "" {
		t.Fatalf("chat request must not send X-Harness-Type, got %q", got)
	}
	if seenBody["model"] != "glm-5.3" {
		t.Fatalf("body model = %v, want glm-5.3", seenBody["model"])
	}
	messages := seenBody["messages"].([]any)
	first := messages[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(first, identityLine) {
		t.Fatalf("system prompt not prefixed: %q", first)
	}
	if strings.Contains(strings.ToLower(first), "you are claude code") {
		t.Fatalf("foreign identity survived: %q", first)
	}
}

// TestRefreshUsesAccountProxy pins that the token exchange honours the
// account's proxy_url. Without it a proxied account still hangs on refresh,
// which is exactly how a "proxied" account looks broken.
func TestRefreshUsesAccountProxy(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"NEW"}}`))
	}))
	defer server.Close()

	payload, _ := EncodeCredential(Credential{AccessToken: "OLD", RefreshToken: "R"})
	store := &memoryStore{
		payload: payload,
		// A closed port: any attempt through this proxy fails at once, which
		// proves the request was routed through it.
		account: accounts.Account{ID: "acct-1", Provider: ProviderID, ProxyURL: "socks5://127.0.0.1:1"},
	}
	client := NewClient(store, RegionCN)
	client.userapiBase = server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.forceRefresh(ctx, "acct-1", Credential{AccessToken: "OLD", RefreshToken: "R"})
	if err == nil {
		t.Fatal("refresh ignored the account proxy and reached the upstream directly")
	}
	if hits != 0 {
		t.Fatalf("refresh bypassed the account proxy: upstream saw %d requests", hits)
	}
}

// TestRefreshSendsAccessToken: the upstream requires an Authorization header on
// /refresh even though the body carries the refresh_token. Without it every
// refresh answers {"code":400000,"msg":"用户未登录，请重新登录"} - indistinguishable
// from a dead refresh token, which sends people hunting for a credential problem
// that does not exist. Verified against the live upstream 2026-10-30:
//
//	valid access token + signature      -> 400000 (user not logged in)
//	valid access token, no signature    -> 400002 (signature rejected)
//	invalid access token + signature    -> 400000
//
// i.e. the signature is checked first, then the identity.
func TestRefreshSendsAccessToken(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"NEW","refresh_token":"R2"}}`))
	}))
	defer server.Close()

	payload, _ := EncodeCredential(Credential{AccessToken: "OLD", RefreshToken: "R1"})
	store := &memoryStore{payload: payload, account: accounts.Account{ID: "acct-1", Provider: ProviderID}}
	client := NewClient(store, RegionCN)
	client.userapiBase = server.URL

	if _, err := client.forceRefresh(context.Background(), "acct-1", Credential{AccessToken: "OLD", RefreshToken: "R1"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("refresh issued no request")
	}
	// The header must carry the CURRENT access token, not be absent.
	if want := "Bearer OLD"; seen[0] != want {
		t.Fatalf("Authorization = %q, want %q", seen[0], want)
	}
	// And the rotated refresh token must be persisted: it is single-use upstream,
	// so losing it forces a full re-login.
	_, saved, err := store.LoadCredentialPayload(context.Background(), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := DecodeCredential(saved)
	if err != nil {
		t.Fatal(err)
	}
	if cred.RefreshToken != "R2" {
		t.Fatalf("stored refresh token = %q, want the rotated R2", cred.RefreshToken)
	}
	if cred.AccessToken != "NEW" {
		t.Fatalf("stored access token = %q, want NEW", cred.AccessToken)
	}
}

// TestProbeReportsReadyOnCatalogSuccess: a catalog hit means the credential
// works.
func TestProbeReportsReadyOnCatalogSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "model-config") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"zai_auto","name":"Auto"},{"id":"zaicoding_glm-5.3","name":"GLM-5.3"}]}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, store := newTestClient(t, server)
	health, err := client.Probe(context.Background(), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if !health.Ready {
		t.Fatalf("probe reported not ready: %+v", health)
	}
	if len(store.observed) == 0 || store.observed[len(store.observed)-1] != "ready" {
		t.Fatalf("observe did not record ready: %v", store.observed)
	}
}

// TestCatalogParsesFlatResponse: the live catalog endpoint returns a FLAT
// {"models":[...]} document, not the {code,msg,data} envelope the userapi
// endpoints use. Reading it as an envelope yields an empty Data, so every
// account advertised only the two static models while the sync reported
// success - a failure that looks healthy in the console.
func TestCatalogParsesFlatResponse(t *testing.T) {
	body := `{"models":[
		{"id":"zai_auto","name":"Auto","reasoning":true,"input":["text"],"contextWindow":1048576,"maxTokens":393216},
		{"id":"zai_auto-fast","name":"Auto-Fast","input":["text","image"],"contextWindow":1048576,"maxTokens":131072},
		{"id":"zaicoding_glm-5.3","name":"GLM-5.3","reasoning":true,"input":["text"],"contextWindow":1048576,"maxTokens":307200},
		{"id":"zai_glm-5.3-flash","name":"GLM-5.3-Flash","input":["text","image"],"contextWindow":1048576,"maxTokens":131072},
		{"id":"zai_glm-5.3-flashx-legal","name":"GLM-5.3-FlashX-Legal","input":["text","image"],"contextWindow":1048576,"maxTokens":307200}
	]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "model-config") {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, _ := newTestClient(t, server)
	models, err := client.Models(context.Background(), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 5 {
		t.Fatalf("parsed %d models from the flat catalog, want 5 (static fallback has 2)", len(models))
	}
	// zai_auto is the model the real desktop client uses as its primary; it
	// only ever appears in the remote catalog, never in the static table.
	if models[0].PublicModel != "auto" {
		t.Fatalf("first public model = %q, want auto", models[0].PublicModel)
	}
	if models[0].Capabilities.MaxOutput != 393216 {
		t.Fatalf("zai_auto max output = %d, want 393216", models[0].Capabilities.MaxOutput)
	}
	// Image capability is expressed as {"input":["text","image"]}.
	if !models[1].Capabilities.Images {
		t.Fatal("zai_auto-fast must report image support from input[]")
	}
}

// TestCatalogParsesRouteIds: the remote catalog advertises prefixed route ids,
// and the public name must be the stripped form.
func TestCatalogParsesRouteIds(t *testing.T) {
	raw := json.RawMessage(`{"models":[{"id":"zai_auto","name":"Auto"},{"id":"zaicoding_glm-5.3","name":"GLM-5.3"}]}`)
	models := parseCatalog(raw)
	if len(models) != 2 {
		t.Fatalf("parsed %d models, want 2", len(models))
	}
	if models[0].PublicModel != "auto" {
		t.Fatalf("public model = %q, want auto", models[0].PublicModel)
	}
	if models[1].PublicModel != "glm-5.3" {
		t.Fatalf("public model = %q, want glm-5.3", models[1].PublicModel)
	}
}

// TestCatalogFallsBackToStatic keeps a catalog hiccup from taking an account
// offline.
func TestCatalogFallsBackToStatic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client, _ := newTestClient(t, server)
	models, err := client.Models(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("a catalog failure must not be an error: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("static fallback is empty")
	}
	if models[0].PublicModel != "glm-5.3" {
		t.Fatalf("first static model = %q", models[0].PublicModel)
	}
}

// TestBalanceIsAbsoluteNotPercentage: credits have no ceiling, so reporting a
// percentage would make routing think an account is nearly spent.
func TestBalanceIsAbsoluteNotPercentage(t *testing.T) {
	raw := json.RawMessage(`{"total_balance":1234.5}`)
	if got := parseBalance(raw); got != 1234.5 {
		t.Fatalf("balance = %v, want 1234.5", got)
	}
	nested := json.RawMessage(`{"wallets":[{"balance":100},{"balance":50}]}`)
	if got := parseBalance(nested); got != 150 {
		t.Fatalf("nested balance = %v, want 150", got)
	}
}

// TestClassifyMapsStatuses pins the taxonomy: 429 -> quota, 401 -> auth,
// 406/403 -> unavailable (not fatal).
func TestClassifyMapsStatuses(t *testing.T) {
	cases := map[int]string{
		401: accounts.KindAuth,
		429: accounts.KindQuota,
		406: accounts.KindUnavailable,
		403: accounts.KindUnavailable,
	}
	for status, want := range cases {
		got := classifier{}.Classify(status, "")
		if got.Kind != want {
			t.Fatalf("status %d -> %q, want %q", status, got.Kind, want)
		}
	}
}

// TestToolCallsForceFinishReason: a tool call reported as "stop" makes the
// agent think the model finished with no content.
func TestToolCallsForceFinishReason(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	outcome, err := collectChatSSE(strings.NewReader(sse), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", outcome.FinishReason)
	}
	if len(outcome.ToolCalls) == 0 {
		t.Fatal("tool calls were dropped")
	}
}

// TestNewlineOnlyChunksAreDropped: this upstream stamps every frame with role,
// including heartbeat frames, so role must not count as payload.
func TestNewlineOnlyChunksAreDropped(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"\n"}}]}`,
		`data: {"choices":[{"delta":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	outcome, err := collectChatSSE(strings.NewReader(sse), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Content != "hello" {
		t.Fatalf("content = %q, want hello", outcome.Content)
	}
}

// TestAdapterSurface pins the capability set: no Login, because the desktop
// OAuth round cannot run on a server.
func TestAdapterSurface(t *testing.T) {
	client := NewClient(&memoryStore{}, RegionCN)
	adapter := client.Adapter()
	if adapter.ID != ProviderID {
		t.Fatalf("adapter id = %q", adapter.ID)
	}
	for _, capability := range []string{"credential", "chat", "models", "classifier", "import_export", "prober"} {
		if !adapter.Supports(capability) {
			t.Fatalf("adapter missing capability %q", capability)
		}
	}
	if adapter.Supports("login") {
		t.Fatal("autoclaw must not advertise a server-side login flow")
	}
}

// TestImporterNormalizesRegion keeps a pasted credential bound to the adapter's
// region, since the desktop file carries no region marker.
func TestImporterNormalizesRegion(t *testing.T) {
	got, err := importer{region: RegionIntl}.PrepareImport([]byte(`{"token":"T"}`))
	if err != nil {
		t.Fatal(err)
	}
	cred, err := DecodeCredential(got.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Region != "intl" {
		t.Fatalf("region = %q, want intl", cred.Region)
	}
	if cred.Format != IntlCredentialFormat {
		t.Fatalf("format = %q, want %q", cred.Format, IntlCredentialFormat)
	}
}

// TestAdapterCredentialImplementsImporter pins a wiring bug that made every
// import fail with "unsupported account import format": the console reaches the
// importer through adapter.Credential, so a codec that does not also implement
// CredentialImporter fails the type assertion even though the provider and its
// format are both registered.
func TestAdapterCredentialImplementsImporter(t *testing.T) {
	for _, region := range []Region{RegionCN, RegionIntl} {
		adapter := NewClient(&memoryStore{}, region).Adapter()
		importer, ok := adapter.Credential.(providers.CredentialImporter)
		if !ok {
			t.Fatalf("region %s: adapter.Credential does not implement CredentialImporter", region)
		}
		if importer.Format() != region.CredentialFormat() {
			t.Fatalf("region %s: importer format = %q, want %q", region, importer.Format(), region.CredentialFormat())
		}
	}
}
