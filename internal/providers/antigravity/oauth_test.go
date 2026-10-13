package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// --- credential modes -------------------------------------------------------

func TestCredentialModeDerivedFromSecrets(t *testing.T) {
	keyMode, err := DecodeCredential([]byte(`{"api_key":"AIza-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if keyMode.IsOAuth() || keyMode.Mode() != AuthModeKey {
		t.Fatalf("key credential resolved to mode %q", keyMode.Mode())
	}
	oauthMode, err := DecodeCredential([]byte(`{"refresh_token":"1//0g-test","access_token":"ya29.a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !oauthMode.IsOAuth() || oauthMode.Mode() != AuthModeOAuth {
		t.Fatalf("oauth credential resolved to mode %q", oauthMode.Mode())
	}
	if oauthMode.Format != CredentialFormatOAuth {
		t.Fatalf("oauth format = %q", oauthMode.Format)
	}
}

func TestCredentialEncodeDropsOtherModeFields(t *testing.T) {
	credential := Credential{
		AuthMode:     AuthModeOAuth,
		RefreshToken: "1//0g-test",
		APIKey:       "AIza-should-be-dropped",
		ProjectID:    "proj-1",
	}
	encoded, err := credential.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["api_key"] != nil && decoded["api_key"] != "" {
		t.Fatalf("oauth encode kept api_key: %v", decoded["api_key"])
	}
	if decoded["refresh_token"] != "1//0g-test" {
		t.Fatalf("refresh token lost: %v", decoded["refresh_token"])
	}
	if decoded["format"] != CredentialFormatOAuth {
		t.Fatalf("format = %v", decoded["format"])
	}
}

func TestValidateOAuthCredentialRejectsKeyCredential(t *testing.T) {
	if err := ValidateOAuthCredential([]byte(`{"api_key":"AIza-test"}`)); err == nil {
		t.Fatal("a key credential must not validate as an oauth credential")
	}
	if err := ValidateCredential([]byte(`{"api_key":"AIza-test"}`)); err != nil {
		t.Fatalf("key credential rejected: %v", err)
	}
	if err := ValidateCredential([]byte(`{"refresh_token":"1//0g-test"}`)); err != nil {
		t.Fatalf("oauth credential rejected by the general validator: %v", err)
	}
}

// --- token endpoint --------------------------------------------------------

func TestRefreshAccessTokenSendsUnderscoredGrantType(t *testing.T) {
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"ya29.new","expires_in":3599,"token_type":"Bearer"}`))
	}))
	defer server.Close()
	withTokenEndpoint(t, server.URL)

	client := NewClient(nil)
	token, err := client.refreshAccessToken(context.Background(), "", "1//0g-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "ya29.new" {
		t.Fatalf("access token = %q", token.AccessToken)
	}
	if gotForm.Get("grant_type") != "refresh_token" {
		t.Fatalf("grant_type = %q (must be the underscored form)", gotForm.Get("grant_type"))
	}
	if gotForm.Get("refresh_token") != "1//0g-refresh" {
		t.Fatalf("refresh_token = %q", gotForm.Get("refresh_token"))
	}
}

func TestRefreshAccessTokenInvalidGrantIsPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	}))
	defer server.Close()
	withTokenEndpoint(t, server.URL)

	client := NewClient(nil)
	_, err := client.refreshAccessToken(context.Background(), "", "1//0g-dead")
	if err == nil {
		t.Fatal("invalid_grant must fail")
	}
	if !strings.Contains(err.Error(), "重新登录") {
		t.Fatalf("message should tell the operator to re-authorize: %v", err)
	}
}

func TestRefreshKeepsOldRefreshTokenWhenUpstreamOmitsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"ya29.new","expires_in":3600}`))
	}))
	defer server.Close()
	withTokenEndpoint(t, server.URL)

	client := NewClient(nil)
	token, err := client.refreshAccessToken(context.Background(), "", "1//0g-keep")
	if err != nil {
		t.Fatal(err)
	}
	if token.RefreshToken != "" {
		t.Fatalf("a missing refresh_token must stay empty, not be invented: %q", token.RefreshToken)
	}
}

// --- authorize URL ---------------------------------------------------------

func TestBuildAuthorizeURLCarriesEveryRequiredParameter(t *testing.T) {
	raw := buildAuthorizeURL("http://127.0.0.1:9000/oauth-callback", "state-1")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Host != "accounts.google.com" {
		t.Fatalf("authorize host = %q", parsed.Host)
	}
	for key, want := range map[string]string{
		"client_id":              oauthClientID,
		"redirect_uri":           "http://127.0.0.1:9000/oauth-callback",
		"response_type":          "code",
		"access_type":            "offline",
		"prompt":                 "consent",
		"include_granted_scopes": "true",
		"state":                  "state-1",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	scopes := query.Get("scope")
	for _, required := range []string{"openid", "cloud-platform", "userinfo.email", "cclog"} {
		if !strings.Contains(scopes, required) {
			t.Fatalf("scope missing %q: %q", required, scopes)
		}
	}
}

// --- login rounds ----------------------------------------------------------

func TestCompleteLoginRejectsMismatchedState(t *testing.T) {
	client := NewClient(nil)
	if _, err := client.StartLogin(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	callback := "http://127.0.0.1:1/oauth-callback?state=not-the-state&code=abc"
	err := client.CompleteLogin(context.Background(), "acct-1", callback)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("mismatched state must be rejected, got %v", err)
	}
}

func TestCompleteLoginSurfacesGoogleError(t *testing.T) {
	client := NewClient(nil)
	if _, err := client.StartLogin(context.Background(), "acct-2"); err != nil {
		t.Fatal(err)
	}
	state := client.roundState("acct-2")
	callback := "http://127.0.0.1:1/oauth-callback?state=" + state + "&error=access_denied&error_description=user+said+no"
	err := client.CompleteLogin(context.Background(), "acct-2", callback)
	if err == nil || !strings.Contains(err.Error(), "user said no") {
		t.Fatalf("Google's refusal must be surfaced, got %v", err)
	}
}

func TestStartLoginReturnsConsentURLWithState(t *testing.T) {
	client := NewClient(nil)
	session, err := client.StartLogin(context.Background(), "acct-3")
	if err != nil {
		t.Fatal(err)
	}
	if session.State == "" || !strings.Contains(session.AuthURL, session.State) {
		t.Fatalf("session = %+v", session)
	}
	if !strings.Contains(session.AuthURL, "oauth-callback") {
		t.Fatalf("authorize URL must carry the loopback redirect: %s", session.AuthURL)
	}
}

func TestPollLoginWithoutRoundFails(t *testing.T) {
	client := NewClient(nil)
	if _, _, err := client.PollLogin(context.Background(), "missing"); err == nil {
		t.Fatal("polling a round that was never started must fail")
	}
}

// --- v1internal request shape ----------------------------------------------

func TestChatRequestBodyCarriesProjectOnlyWhenSet(t *testing.T) {
	req := translate.ChatRequest{
		Model:    "gemini-3-flash",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	}
	raw, err := chatRequestBody(req, "gemini-3-flash", "proj-9")
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["project"] != "proj-9" {
		t.Fatalf("project = %v", envelope["project"])
	}
	if envelope["model"] != "gemini-3-flash" {
		t.Fatalf("model = %v", envelope["model"])
	}
	if envelope["userAgent"] != "antigravity" {
		t.Fatalf("userAgent = %v", envelope["userAgent"])
	}
	// requestType must not be sent on the agent path: both references disagree
	// about it and sending one risks a spurious 429.
	if _, present := envelope["requestType"]; present {
		t.Fatal("requestType must be absent on the agent path")
	}
	withoutProject, err := chatRequestBody(req, "gemini-3-flash", "")
	if err != nil {
		t.Fatal(err)
	}
	var bare map[string]any
	if err := json.Unmarshal(withoutProject, &bare); err != nil {
		t.Fatal(err)
	}
	if _, present := bare["project"]; present {
		t.Fatal("project key must be omitted when unknown")
	}
}

func TestContentsOfEndsWithUserAndMapsToolResponses(t *testing.T) {
	messages := []translate.ChatMessage{
		{Role: "assistant", Content: "thinking out loud"},
		{Role: "tool", ToolCallID: "call-1", Content: `{"ok":true}`},
	}
	contents := contentsOf(messages)
	if len(contents) == 0 {
		t.Fatal("contents must not be empty")
	}
	if roleOf(contents[0]) != "user" {
		t.Fatalf("first turn must be user, got %q", roleOf(contents[0]))
	}
	if roleOf(contents[len(contents)-1]) != "user" {
		t.Fatalf("last turn must be user, got %q", roleOf(contents[len(contents)-1]))
	}
	// The functionResponse must ride a user turn, never a model turn.
	for _, turn := range contents {
		parts, _ := turn.(map[string]any)["parts"].([]any)
		for _, part := range parts {
			if _, ok := part.(map[string]any)["functionResponse"]; ok {
				if roleOf(turn) != "user" {
					t.Fatalf("functionResponse rode a %q turn", roleOf(turn))
				}
			}
		}
	}
}

func TestThinkingBudgetKeepsOutputAboveBudget(t *testing.T) {
	req := translate.ChatRequest{
		MaxTokens:       json.RawMessage(`1000`),
		ReasoningEffort: json.RawMessage(`"high"`),
	}
	config := generationConfig(req)
	thinking, ok := config["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatal("thinkingConfig missing for a high-effort request")
	}
	budget := thinking["thinkingBudget"]
	maxOutput, _ := config["maxOutputTokens"]
	budgetNumber, _ := budget.(int)
	outputNumber, _ := maxOutput.(int)
	if budgetNumber <= 0 || outputNumber <= budgetNumber {
		t.Fatalf("budget=%v maxOutput=%v: output must exceed the thinking budget", budget, maxOutput)
	}
}

func TestThinkingBudgetAbsentWhenEffortIsNone(t *testing.T) {
	config := generationConfig(translate.ChatRequest{ReasoningEffort: json.RawMessage(`"none"`)})
	if _, present := config["thinkingConfig"]; present {
		t.Fatal("no thinkingConfig may be sent when thinking is off")
	}
}

func TestFunctionDeclarationsDropOnToolChoiceNone(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"do","description":"d","parameters":{"type":"object"}}}]`)
	if got := functionDeclarations(tools, json.RawMessage(`"none"`)); got != nil {
		t.Fatalf("tool_choice=none must drop tools, got %v", got)
	}
	got := functionDeclarations(tools, nil)
	if len(got) != 1 {
		t.Fatalf("declarations = %v", got)
	}
	declaration := got[0].(map[string]any)
	if declaration["name"] != "do" {
		t.Fatalf("name = %v", declaration["name"])
	}
	// An empty object schema gets a placeholder property, which Gemini requires.
	parameters, _ := declaration["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	if len(properties) == 0 {
		t.Fatalf("empty object schema must gain a placeholder property: %v", parameters)
	}
}

// --- v1internal stream -----------------------------------------------------

func TestTranslateGeminiStreamFoldsTextThoughtAndTools(t *testing.T) {
	body := strings.Join([]string{
		`data: {"response":{"responseId":"r1","candidates":[{"content":{"parts":[{"text":"answer"}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"},"id":"call-9"}}]}}]}}`,
		`data: {"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"totalTokenCount":17}}}`,
		`data: [DONE]`,
	}, "\n")
	var out strings.Builder
	if err := translateGeminiStream(strings.NewReader(body), &out, "gemini-3-flash"); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, `"content":"answer"`) {
		t.Fatalf("content delta missing: %s", text)
	}
	if !strings.Contains(text, `"reasoning_content":"hmm"`) {
		t.Fatalf("thought delta must map to reasoning_content: %s", text)
	}
	if !strings.Contains(text, `"tool_calls"`) || !strings.Contains(text, `"name":"lookup"`) {
		t.Fatalf("tool call delta missing: %s", text)
	}
	// A tool call that ended on STOP must report tool_calls.
	if !strings.Contains(text, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason must be tool_calls: %s", text)
	}
	if !strings.Contains(text, `"completion_tokens":7`) {
		t.Fatalf("completion tokens must include thoughts: %s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("stream must end with [DONE]: %s", text)
	}
}

func TestTranslateGeminiStreamTruncationIsAnError(t *testing.T) {
	// No finishReason and no [DONE]: the response was cut off mid-stream.
	body := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}}`
	var out strings.Builder
	if err := translateGeminiStream(strings.NewReader(body), &out, "gemini-3-flash"); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, `"error"`) {
		t.Fatalf("a truncated stream must surface an error frame: %s", text)
	}
	if !strings.Contains(text, "截断") {
		t.Fatalf("error should name the truncation: %s", text)
	}
}

func TestTranslateGeminiStreamSurfacesUpstreamError(t *testing.T) {
	body := `data: {"response":{"error":{"code":429,"message":"quota exceeded"}}}`
	var out strings.Builder
	if err := translateGeminiStream(strings.NewReader(body), &out, "gemini-3-flash"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "quota exceeded") {
		t.Fatalf("upstream error must reach the client: %s", out.String())
	}
}

// --- v1 catalog ------------------------------------------------------------

func TestParseV1CatalogKeepsOnlyGeminiFamily(t *testing.T) {
	raw := []byte(`{"models":{
		"gemini-3-flash":{"displayName":"Gemini 3 Flash","supportsThinking":true,"thinkingBudget":32768,"maxTokens":1048576,"maxOutputTokens":65536},
		"claude-sonnet-4-6":{"displayName":"Claude","supportsThinking":true},
		"gpt-oss-120b":{"displayName":"GPT-OSS"}
	}}`)
	models := parseV1Catalog(raw)
	if len(models) != 1 {
		t.Fatalf("only the gemini family may be advertised, got %d models", len(models))
	}
	model := models[0]
	if model.NativeModel != "gemini-3-flash" {
		t.Fatalf("model = %q", model.NativeModel)
	}
	if !model.Capabilities.Reasoning {
		t.Fatal("supportsThinking must carry through")
	}
	if model.Capabilities.ContextWindow != 1048576 {
		t.Fatalf("context window = %d", model.Capabilities.ContextWindow)
	}
	if len(model.Capabilities.ReasoningOptions) == 0 {
		t.Fatal("a thinking model must declare reasoning options")
	}
}

func TestUpstreamModelIDAppliesAliasesAndTieredRule(t *testing.T) {
	for public, want := range map[string]string{
		"gemini-3.6-flash":     "gemini-3.6-flash-tiered",
		"gemini-3.1-pro-high":  "gemini-pro-agent",
		"gemini-3.7-flash-low": "gemini-3.7-flash-low", // already carries a suffix
		"gemini-2.5-flash":     "gemini-2.5-flash",     // below 3.6, no -tiered
	} {
		if got := upstreamModelID(public); got != want {
			t.Fatalf("upstreamModelID(%q) = %q, want %q", public, got, want)
		}
	}
}

// --- user agent ------------------------------------------------------------

func TestSanitizeUserAgentEnforcesAntigravityFloor(t *testing.T) {
	if got := sanitizeUserAgent("Antigravity/4.0.0 (X11) Chrome/1"); got == "" {
		t.Fatal("a below-floor version must be raised, not rejected")
	} else if !strings.Contains(got, knownStableVersion) {
		t.Fatalf("version must be raised to the floor: %q", got)
	}
	if got := sanitizeUserAgent("SomeOtherTool/1.0"); got != "" {
		t.Fatalf("a UA without antigravity must be rejected: %q", got)
	}
	if got := sanitizeUserAgent("Antigravity/4.3.0 (X11)"); !strings.Contains(got, "4.3.0") {
		t.Fatalf("a valid UA must pass through: %q", got)
	}
}

func TestMethodURLUsesColonSyntax(t *testing.T) {
	got := methodURL(v1BaseProd, methodFetchModels)
	want := v1BaseProd + ":fetchAvailableModels"
	if got != want {
		t.Fatalf("methodURL = %q, want %q", got, want)
	}
	if !strings.HasSuffix(streamGenerateURL(v1BaseProd), ":streamGenerateContent?alt=sse") {
		t.Fatalf("stream URL = %q", streamGenerateURL(v1BaseProd))
	}
	if !strings.Contains(loadCodeAssistURL(), "cloudcode-pa.googleapis.com") {
		t.Fatalf("project discovery must use prod: %q", loadCodeAssistURL())
	}
}

// --- helpers ---------------------------------------------------------------

// withTokenEndpoint points the Google token endpoint at a test server.
func withTokenEndpoint(t *testing.T, raw string) {
	t.Helper()
	previous := googleTokenURL
	googleTokenURL = raw
	t.Cleanup(func() { googleTokenURL = previous })
}

// --- fake store ------------------------------------------------------------

// fakeStore is the minimal Store used by the login/refresh tests.
type fakeStore struct {
	payloads map[string][]byte
}

func (s *fakeStore) Get(context.Context, string) (accounts.Account, error) {
	return accounts.Account{Provider: "antigravity"}, nil
}

func (s *fakeStore) LoadCredentialPayload(_ context.Context, accountID string) (string, []byte, error) {
	payload, ok := s.payloads[accountID]
	if !ok {
		return "", nil, accounts.ErrAccountNotFound
	}
	return CredentialFormatOAuth, payload, nil
}

func (s *fakeStore) SaveCredentialPayload(_ context.Context, accountID, format string, payload []byte) error {
	if s.payloads == nil {
		s.payloads = map[string][]byte{}
	}
	s.payloads[accountID] = payload
	return nil
}

func (s *fakeStore) Observe(context.Context, string, string, string, string, string) error {
	return nil
}
