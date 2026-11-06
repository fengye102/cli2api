package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// fakeGateway is the model plane (loomyad.xunfei.cn/api/v1).
type fakeGateway struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	session   string
	chatCalls int
	chatBody  map[string]any
	chatSeen  []string

	// failFirstChat answers the first chat call with 401 and then behaves,
	// which is the "session died early" case.
	failFirstChat bool
	chatFailures  int

	// envelopeAuth answers a dead session the way the live gateway does - HTTP
	// 200 with the 100002 business envelope - instead of a 401. Both shapes
	// exist in the wild, so both are covered.
	envelopeAuth bool

	tasks map[string]bool
	// imageBody/imageCalls/imageStatus record what the image plane received.
	imageBody   map[string]any
	imageCalls  int
	imageStatus int
	// balance is the credit balance /points/records reports; completing a task
	// adds that task's points so a test can assert the claim reports what
	// actually landed instead of only the advertised sum.
	balance float64
	// creditOnClaim=false models a gateway that marks the task complete and
	// credits nothing, which is what the live service seems to do when the task
	// itself was never performed.
	creditOnClaim bool
}

func newFakeGateway(t *testing.T, session string) *fakeGateway {
	gateway := &fakeGateway{t: t, session: session, tasks: map[string]bool{}, envelopeAuth: true, balance: 9989, creditOnClaim: true}
	gateway.srv = httptest.NewServer(http.HandlerFunc(gateway.handle))
	t.Cleanup(gateway.srv.Close)
	return gateway
}

func (g *fakeGateway) url() string { return g.srv.URL + "/api/v1" }

func (g *fakeGateway) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	session := g.session
	g.mu.Unlock()

	if r.Header.Get("traceparent") == "" {
		http.Error(w, `{"error":{"message":"missing traceparent"}}`, http.StatusBadRequest)
		return
	}
	if r.Header.Get("loomy-version") == "" {
		http.Error(w, `{"error":{"message":"missing loomy-version"}}`, http.StatusBadRequest)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+session || r.Header.Get("token") != session {
		if g.envelopeAuth {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":"100002","desc":"登录已失效，请重新登录","data":{}}`)
			return
		}
		http.Error(w, `{"error":{"message":"Unauthorized"}}`, http.StatusUnauthorized)
		return
	}

	switch r.URL.Path {
	case "/api/v1/images/generations":
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		g.mu.Lock()
		g.imageBody = body
		g.imageCalls++
		status := g.imageStatus
		g.mu.Unlock()
		if status >= 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"message":"insufficient points","type":"quota_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"created":1793948356,"data":[{"url":"https://example.test/out.png?signature=abc"}],"points_consumed":110}`)

	case "/api/v1/chat/completions":
		g.mu.Lock()
		g.chatCalls++
		g.chatSeen = append(g.chatSeen, r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		g.chatBody = body
		fail := g.failFirstChat && g.chatFailures == 0
		if fail {
			g.chatFailures++
		}
		g.mu.Unlock()
		if fail {
			http.Error(w, `{"error":{"message":"session expired"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你好\",\"reasoning_content\":\"想\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"世界\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7,\"points_consumed\":0.8}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")

	case "/api/v1/models":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[
			{"id":"qwen3.8-flash","name":"qwen 3.8 flash (x0.8)","multiplier":0.8,"context_length":1000000,"modalities":["text","image","video"]},
			{"id":"spark-x","name":"Spark X2.5 (x0.1)","multiplier":0.1,"context_length":1048576,"modalities":["text"]},
			{"id":"Hy-Image-3.5-preview","name":"Hy image 3.5 preview","multiplier":0,"context_length":100000,"modalities":["text","image"],"type":"image"},
			{"id":"doubao-seedream-5-lite","name":"Doubao seedream 5 lite","multiplier":0,"context_length":128000,"type":"image"},
			{"id":"qwen-image-3.0-pro","name":"qwen image 3.0 pro","multiplier":0,"type":"image"}
		]}`)

	case "/api/v1/points/records":
		w.Header().Set("Content-Type", "application/json")
		g.mu.Lock()
		balance := g.balance
		g.mu.Unlock()
		fmt.Fprintf(w, `{"data":{"balance":%.0f,"dailyBalance":4989,"availableBalance":%.0f}}`, balance, balance)

	case "/api/v1/onboarding/tasks":
		w.Header().Set("Content-Type", "application/json")
		g.mu.Lock()
		tasks := map[string]bool{}
		for key, done := range g.tasks {
			tasks[key] = done
		}
		g.mu.Unlock()
		fmt.Fprintf(w, `{"code":"000000","data":{"tasks":%s,"earned":0,"total":10000}}`, string(mustJSON(g.t, tasks)))

	case "/api/v1/onboarding/tasks/complete":
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(raw, &body)
		g.mu.Lock()
		already := g.tasks[body.Key]
		g.tasks[body.Key] = true
		if !already && g.creditOnClaim {
			if task, ok := onboardingTaskByKey(body.Key); ok {
				g.balance += float64(task.Points)
			}
		}
		balance := g.balance
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":"000000","data":{"alreadyCompleted":%v,"balance":%.0f}}`, already, balance)

	default:
		http.NotFound(w, r)
	}
}

// fakeAccountService is the account plane (account.xfinfr.com).
type fakeAccountService struct {
	t   *testing.T
	srv *httptest.Server
	key testRSAKey

	mu            sync.Mutex
	password      string
	issued        int
	smsCode       string
	smsMsgID      string
	lastSignature string
}

func newFakeAccountService(t *testing.T, password string) *fakeAccountService {
	service := &fakeAccountService{t: t, key: newTestRSAKey(t), password: password, smsCode: "123456", smsMsgID: "msg-1"}
	service.srv = httptest.NewServer(http.HandlerFunc(service.handle))
	t.Cleanup(service.srv.Close)
	return service
}

func (s *fakeAccountService) url() string { return s.srv.URL }

func (s *fakeAccountService) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.lastSignature = r.Header.Get("Authorization")
	s.mu.Unlock()

	if !strings.HasPrefix(r.Header.Get("Authorization"), "account "+accessKeyID+":") {
		http.Error(w, `{"code":"100002","desc":"signature rejected"}`, http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	body := struct {
		Param map[string]any `json:"param"`
	}{}
	_ = json.Unmarshal(raw, &body)

	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case accountPukeyPath:
		fmt.Fprintf(w, `{"code":"000000","data":{"pukey":%q,"rcode":"rc-1"}}`, s.key.publicB64)

	case accountPwdLoginPath:
		encrypted, _ := body.Param["password"].(string)
		ciphertext, err := base64Decode(encrypted)
		if err != nil {
			http.Error(w, `{"code":"100001","desc":"bad ciphertext"}`, http.StatusOK)
			return
		}
		if plain := s.key.decrypt(s.t, ciphertext); plain != s.password {
			fmt.Fprint(w, `{"code":"100001","desc":"密码错误"}`)
			return
		}
		s.mu.Lock()
		s.issued++
		session := fmt.Sprintf("sess-%d", s.issued)
		s.mu.Unlock()
		fmt.Fprintf(w, `{"code":"000000","data":{"session":%q,"userid":"u-1","phone":"13800138000"}}`, session)

	case accountSendMsgPath:
		fmt.Fprintf(w, `{"code":"000000","data":{"msgid":%q}}`, s.smsMsgID)

	case accountCheckCodePath:
		code, _ := body.Param["mcode"].(string)
		if code != s.smsCode {
			fmt.Fprint(w, `{"code":"100001","desc":"验证码错误"}`)
			return
		}
		s.mu.Lock()
		s.issued++
		session := fmt.Sprintf("sess-%d", s.issued)
		s.mu.Unlock()
		fmt.Fprintf(w, `{"code":"000000","data":{"session":%q,"userid":"u-sms"}}`, session)

	default:
		http.NotFound(w, r)
	}
}

func base64Decode(value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("empty")
	}
	return decodeB64(value)
}

func newTestClient(t *testing.T, store *fakeStore, gateway *fakeGateway, account *fakeAccountService) *Client {
	client := NewClient(store)
	client.modelBase = gateway.url()
	client.accountBase = account.url()
	return client
}

func TestChatNonStreamAggregatesContentAndCredits(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	outcome, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{Model: "loomy/qwen3.8-flash"})
	if err != nil {
		t.Fatalf("ChatNonStream: %v", err)
	}
	if outcome.Content != "你好世界" {
		t.Fatalf("content = %q", outcome.Content)
	}
	if outcome.Reasoning != "想" {
		t.Fatalf("reasoning = %q", outcome.Reasoning)
	}
	if outcome.Credits == nil || *outcome.Credits != 0.8 {
		t.Fatalf("credits = %v", outcome.Credits)
	}
	if outcome.PromptTokens != 5 || outcome.CompletionTokens != 7 {
		t.Fatalf("tokens = %d/%d", outcome.PromptTokens, outcome.CompletionTokens)
	}
	gateway.mu.Lock()
	body := gateway.chatBody
	gateway.mu.Unlock()
	if body["model"] != "qwen3.8-flash" {
		t.Fatalf("upstream model = %v", body["model"])
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v", body["stream"])
	}
}

func TestChatStreamRelaysUpstreamBody(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	resp, _, err := client.ChatStream(context.Background(), "acc-1", translate.ChatRequest{Model: "spark-x"})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "你好") {
		t.Fatalf("relayed body = %q", raw)
	}
}

func TestChatRelogsInOnceAfterUnauthorized(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{
		Format: CredentialFormat, LoginID: "13800138000", Password: "pw", Session: "sess-stale", ExpiresAt: farFuture(),
	})
	// The stale session makes the gateway answer 401 on the first call; the
	// adapter must log in again and retry exactly once.
	gateway := newFakeGateway(t, "sess-1")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	outcome, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{Model: "qwen3.8-flash"})
	if err != nil {
		t.Fatalf("ChatNonStream after relogin: %v", err)
	}
	if outcome.Content == "" {
		t.Fatal("no content after relogin")
	}
	stored := store.credential(t, "acc-1")
	if stored.Session != "sess-1" {
		t.Fatalf("stored session = %q, want the renewed one", stored.Session)
	}
	if stored.DeviceID == "" {
		t.Fatal("a login must mint and persist the device id")
	}
	gateway.mu.Lock()
	calls := gateway.chatCalls
	gateway.mu.Unlock()
	if calls != 1 {
		t.Fatalf("chat calls = %d, want 1 (the retry, after one rejected attempt)", calls)
	}
}

func TestChatHandlesBothAuthShapes(t *testing.T) {
	for _, envelope := range []bool{true, false} {
		t.Run(fmt.Sprintf("envelope=%v", envelope), func(t *testing.T) {
			store := newFakeStore()
			store.add("acc-1", Credential{
				Format: CredentialFormat, LoginID: "13800138000", Password: "pw", Session: "sess-stale", ExpiresAt: farFuture(),
			})
			gateway := newFakeGateway(t, "sess-1")
			gateway.envelopeAuth = envelope
			account := newFakeAccountService(t, "pw")
			client := newTestClient(t, store, gateway, account)

			outcome, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{Model: "spark-x"})
			if err != nil {
				t.Fatalf("ChatNonStream: %v", err)
			}
			if outcome.Content == "" {
				t.Fatal("no content after relogin")
			}
			if stored := store.credential(t, "acc-1"); stored.Session != "sess-1" {
				t.Fatalf("stored session = %q", stored.Session)
			}
		})
	}
}

func TestDeadSessionEnvelopeIsClassifiedAsAuth(t *testing.T) {
	// The live gateway answers an expired session with HTTP 200 and
	// {"code":"100002"}. Left unread, that turns into "malformed catalog".
	if err := envelopeError([]byte(`{"code":"100002","desc":"登录已失效，请重新登录","data":{}}`)); err == nil {
		t.Fatal("a 100002 envelope must be an error")
	} else if providerErr, ok := err.(*providers.Error); !ok || providerErr.Kind != "auth" {
		t.Fatalf("error = %v, want an auth-classified provider error", err)
	}
	if err := envelopeError([]byte(`{"data":[{"id":"spark-x"}]}`)); err != nil {
		t.Fatalf("a successful payload must not be treated as an error: %v", err)
	}
	if err := envelopeError([]byte(`{"code":"000000","data":{}}`)); err != nil {
		t.Fatalf("an ok envelope must pass: %v", err)
	}
}

func TestSessionWithoutPasswordReportsAuthInsteadOfHanging(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-stale"})
	gateway := newFakeGateway(t, "sess-fresh")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	_, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{Model: "spark-x"})
	if err == nil {
		t.Fatal("a dead session with no password must fail, not pretend")
	}
	if !strings.Contains(err.Error(), "no password") && !strings.Contains(err.Error(), "password") {
		t.Fatalf("error = %v, want a renewable-credential explanation", err)
	}
}

func TestQuotaMapsCreditWindows(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	quota, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if quota.Remaining != 9989 || quota.Unit != quotaUnit {
		t.Fatalf("quota = %+v", quota)
	}
	if quota.ProviderID != "loomy" {
		t.Fatalf("provider = %q", quota.ProviderID)
	}
	if len(quota.Windows) != 3 {
		t.Fatalf("windows = %d", len(quota.Windows))
	}
	if quota.Exceeded {
		t.Fatal("a funded account must not be marked exhausted")
	}
}

func TestModelsMarkImageGenerationRecords(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	models, err := client.Models(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 5 {
		t.Fatalf("models = %d, want 5 (2 chat + 3 image)", len(models))
	}
	byID := map[string]providers.ModelInfo{}
	for _, model := range models {
		byID[model.PublicModel] = model
		if model.Credits == "" && !model.Free {
			t.Fatalf("model %s has neither a price nor a free flag", model.PublicModel)
		}
	}
	if !byID["loomy/qwen3.8-flash"].Capabilities.Tools || !byID["loomy/spark-x"].Capabilities.Tools {
		t.Fatal("chat models must keep advertising tools")
	}
	// The generation records arrive with no `modalities` field — the shape that
	// used to make them look like chat models. They must now be advertised as
	// image generation and stripped of chat affordances.
	for _, id := range []string{
		"loomy/Hy-Image-3.5-preview", "loomy/doubao-seedream-5-lite", "loomy/qwen-image-3.0-pro",
	} {
		model, ok := byID[id]
		if !ok {
			t.Fatalf("%s is not advertised: %v", id, byID)
		}
		if !model.Capabilities.ImageGeneration || model.Capabilities.Tools {
			t.Fatalf("%s capabilities = %+v, want image generation and no tools", id, model.Capabilities)
		}
	}
	for _, model := range models {
		if model.Capabilities.ImageGeneration && model.PublicModel == "loomy/spark-x" {
			t.Fatal("a text model was marked as image generation")
		}
	}
}

func TestGenerateImageForwardsBodyAndCredits(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	result, err := client.GenerateImage(context.Background(), "acc-1", providers.ImageGenerationRequest{
		PublicModel: "loomy/Hy-Image-3.5-preview",
		Body:        []byte(`{"model":"loomy/Hy-Image-3.5-preview","prompt":"a red apple","size":"1024x1024","n":1}`),
	})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if result.Credits == nil || *result.Credits != 110 {
		t.Fatalf("credits = %v, want the upstream points_consumed 110", result.Credits)
	}
	if !strings.Contains(string(result.Body), "example.test/out.png") {
		t.Fatalf("body = %s", result.Body)
	}
	if got, _ := gateway.imageBody["model"].(string); got != "Hy-Image-3.5-preview" {
		t.Fatalf("upstream model = %q, want the native name without the loomy/ prefix", got)
	}
	if got, _ := gateway.imageBody["size"].(string); got != "1024x1024" {
		t.Fatalf("size was not forwarded: %v", gateway.imageBody)
	}
	if got, _ := gateway.imageBody["prompt"].(string); got != "a red apple" {
		t.Fatalf("prompt was not forwarded: %v", gateway.imageBody)
	}
}

func TestGenerateImageSurfacesUpstreamFailure(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	gateway.imageStatus = http.StatusPaymentRequired
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	_, err := client.GenerateImage(context.Background(), "acc-1", providers.ImageGenerationRequest{
		PublicModel: "loomy/doubao-seedream-5-lite",
		Body:        []byte(`{"model":"loomy/doubao-seedream-5-lite","prompt":"x"}`),
	})
	if err == nil {
		t.Fatal("a 402 from the image plane must not look like success")
	}
	providerErr, ok := err.(*providers.Error)
	if !ok || providerErr.Kind == "" {
		t.Fatalf("err = %v, want a classified provider error", err)
	}
}

// A chat request naming a generation model gets a pointer to the image
// endpoint instead of the upstream's 404 "该模型暂未开放".
func TestChatOnImageModelPointsAtTheImageEndpoint(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	_, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{
		Model:    "loomy/qwen-image-3.0-pro",
		Stream:   true,
		Messages: []translate.ChatMessage{{Role: "user", Content: "画一只猫"}},
	})
	if err == nil {
		t.Fatal("chat on an image model must be refused")
	}
	if !strings.Contains(err.Error(), "/v1/images/generations") {
		t.Fatalf("err = %v, want a pointer to the image endpoint", err)
	}
	if gateway.chatCalls != 0 {
		t.Fatalf("chat reached upstream %d times, want 0", gateway.chatCalls)
	}
}

func TestClaimAllWalksOutstandingTasks(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	gateway.tasks["first_message"] = true
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	plans, err := client.PreviewClaim(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("PreviewClaim: %v", err)
	}
	if plans[0].PlanID != claimAllPlanID {
		t.Fatalf("first plan = %q, want the one-click plan", plans[0].PlanID)
	}
	var claimedStatus string
	for _, plan := range plans {
		if plan.PlanID == taskPlanPrefix+"first_message" {
			claimedStatus = plan.Status
		}
	}
	if claimedStatus != "claimed" {
		t.Fatalf("first_message status = %q", claimedStatus)
	}

	result, err := client.Claim(context.Background(), "acc-1", claimAllPlanID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !result.Claimed {
		t.Fatalf("claim result = %+v", result)
	}
	if !strings.Contains(result.Message, "9500") {
		t.Fatalf("message = %q, want the outstanding tasks' 9500 credits", result.Message)
	}
	for _, task := range onboardingTasks {
		if !gateway.tasks[task.Key] {
			t.Fatalf("task %s was never claimed", task.Key)
		}
	}
}

func TestSingleClaimIsIdempotent(t *testing.T) {
	store := newFakeStore()
	store.add("acc-1", Credential{Format: CredentialFormat, Session: "sess-live"})
	gateway := newFakeGateway(t, "sess-live")
	gateway.tasks["share_soul"] = true
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)

	result, err := client.Claim(context.Background(), "acc-1", taskPlanPrefix+"share_soul")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !result.AlreadyClaimed {
		t.Fatalf("result = %+v, want already-claimed", result)
	}
}

func TestSMSLoginRoundSendsCodeAndStoresSession(t *testing.T) {
	store := newFakeStore()
	gateway := newFakeGateway(t, "sess-1")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)
	store.addAccount("acc-1")

	flow := loginFlow{client: client}
	session, err := flow.StartLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if session.State != "sms" {
		t.Fatalf("state = %q", session.State)
	}
	if _, message, err := flow.PollLogin(context.Background(), "acc-1"); err != nil || !strings.Contains(message, "手机号") {
		t.Fatalf("poll before phone = %q, %v", message, err)
	}

	if err := flow.CompleteLogin(context.Background(), "acc-1", "13800138000"); err != nil {
		t.Fatalf("CompleteLogin(phone): %v", err)
	}
	if _, message, err := flow.PollLogin(context.Background(), "acc-1"); err != nil || !strings.Contains(message, "138****8000") {
		t.Fatalf("poll after phone = %q, %v", message, err)
	}

	if err := flow.CompleteLogin(context.Background(), "acc-1", "123456"); err != nil {
		t.Fatalf("CompleteLogin(code): %v", err)
	}
	cred := store.credential(t, "acc-1")
	if cred.Session != "sess-1" {
		t.Fatalf("stored session = %q", cred.Session)
	}
	if cred.LoginID != "13800138000" {
		t.Fatalf("stored login id = %q", cred.LoginID)
	}
	if cred.ExpiresAt == 0 {
		t.Fatal("the session expiry must be recorded, it is the only renewal signal")
	}
}

func TestSMSLoginRejectsWrongCode(t *testing.T) {
	store := newFakeStore()
	gateway := newFakeGateway(t, "sess-1")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)
	store.addAccount("acc-1")
	flow := loginFlow{client: client}

	if err := flow.CompleteLogin(context.Background(), "acc-1", "13800138000"); err != nil {
		t.Fatalf("phone: %v", err)
	}
	err := flow.CompleteLogin(context.Background(), "acc-1", "000000")
	if err == nil {
		t.Fatal("a wrong code must fail")
	}
}

func TestPasswordPasteLogsInWithoutSMS(t *testing.T) {
	store := newFakeStore()
	gateway := newFakeGateway(t, "sess-1")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)
	store.addAccount("acc-1")
	flow := loginFlow{client: client}

	if err := flow.CompleteLogin(context.Background(), "acc-1", "13800138000 pw"); err != nil {
		t.Fatalf("password paste: %v", err)
	}
	cred := store.credential(t, "acc-1")
	if cred.Session != "sess-1" || !cred.CanRenew() {
		t.Fatalf("credential = %+v", cred)
	}
}
