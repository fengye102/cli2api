package zcode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// TestTrialQuotaNarrowsTheCooldown covers the two refusal wordings a spent Start
// Plan trial produces, and the cases that must keep the account-wide reading.
func TestTrialQuotaNarrowsTheCooldown(t *testing.T) {
	const (
		trialEnded   = `{"code":400,"msg":"ZCode has no active limited quota for GLM-5.3-Trial"}`
		bucketSpent  = `{"code":429,"msg":"ZCode's limited quota for GLM-5.3 is exhausted or temporarily unavailable"}`
		otherBucket  = `{"code":429,"msg":"ZCode's limited quota for GLM-5.2 is exhausted or temporarily unavailable"}`
		accountLevel = `{"code":1005,"msg":"exceed quota limit"}`
	)
	cases := []struct {
		name    string
		model   string
		kind    string
		body    string
		want    string
		comment string
	}{
		{"trial ended, trial requested", "GLM-5.3-Trial", accounts.KindQuota, trialEnded, "GLM-5.3-Trial",
			"the refusal names the entry itself"},
		{"bucket spent, trial requested", "GLM-5.3-Trial", accounts.KindQuota, bucketSpent, "GLM-5.3-Trial",
			"the bucket is the entry less -Trial"},
		{"case-insensitive", "glm-5.3-trial", accounts.KindQuota, trialEnded, "glm-5.3-trial",
			"the upstream capitalises, the request may not"},
		{"another model's bucket", "GLM-5.3-Trial", accounts.KindQuota, otherBucket, "",
			"a different bucket says nothing about this trial"},
		{"paid model, bucket spent", "GLM-5.3", accounts.KindQuota, bucketSpent, "",
			"without -Trial the request is not a trial entry"},
		{"paid model, trial ended", "GLM-5.3-Flash", accounts.KindQuota, trialEnded, "",
			"a refusal about another entry must not cool this model"},
		{"account-level balance", "GLM-5.3-Trial", accounts.KindQuota, accountLevel, "",
			"an account-level refusal stays account-wide"},
		{"not quota at all", "GLM-5.3-Trial", accounts.KindAuth, trialEnded, "",
			"only quota refusals are narrowed"},
		{"empty body", "GLM-5.3-Trial", accounts.KindQuota, "", "",
			"nothing to read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trialQuotaModel(tc.model, tc.kind, tc.body)
			if got != tc.want {
				t.Fatalf("trialQuotaModel(%q) = %q, want %q (%s)", tc.model, got, tc.want, tc.comment)
			}
		})
	}
}

// TestChatNonStreamTrialQuotaStaysOnTheTrial drives the whole path: an upstream
// refusal about a spent trial must reach the executor as a model-scoped quota,
// so the pool rests that entry and the plan's paid models keep serving.
func TestChatNonStreamTrialQuotaStaysOnTheTrial(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		body     string
		status   int
		wantKind string
		wantMod  string
	}{
		{
			name:     "trial ended",
			model:    "GLM-5.3-Trial",
			status:   http.StatusBadRequest,
			body:     `{"code":400,"msg":"ZCode has no active limited quota for GLM-5.3-Trial"}`,
			wantKind: accounts.KindQuota,
			wantMod:  "GLM-5.3-Trial",
		},
		{
			name:     "paid model keeps the account",
			model:    "GLM-5.3",
			status:   http.StatusTooManyRequests,
			body:     `{"code":429,"msg":"ZCode's limited quota for GLM-5.3 is exhausted or temporarily unavailable"}`,
			wantKind: accounts.KindQuota,
			wantMod:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := NewClient(&memStore{items: map[string][]byte{
				"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"oauth","provider":"zai","zcode_jwt_token":"jwt-abc"}`),
			}})
			client.http.Transport = rewriteTransport{server: server.URL, base: client.http.Transport}

			_, err := client.ChatNonStream(context.Background(), "acc1", translate.ChatRequest{
				Model:    tc.model,
				Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
			})
			var providerErr *providers.Error
			if !errors.As(err, &providerErr) {
				t.Fatalf("want a *providers.Error, got %v", err)
			}
			if providerErr.Kind != tc.wantKind {
				t.Fatalf("kind=%q want %q", providerErr.Kind, tc.wantKind)
			}
			if providerErr.Model != tc.wantMod {
				t.Fatalf("model scope=%q want %q", providerErr.Model, tc.wantMod)
			}
		})
	}
}

// TestEnvelopeTrialQuotaStaysOnTheTrial covers the 2xx envelope shape, which the
// gateway also uses for a spent entitlement.
func TestEnvelopeTrialQuotaStaysOnTheTrial(t *testing.T) {
	body := []byte(`{"code":1005,"msg":"exceed quota limit: no active limited quota for GLM-5.3-Trial"}`)
	classified, ok := zcodeEnvelopeError("GLM-5.3-Trial", http.StatusOK, body)
	if !ok {
		t.Fatalf("envelope not detected: %s", body)
	}
	if classified.Kind != accounts.KindQuota {
		t.Fatalf("kind=%q want %q", classified.Kind, accounts.KindQuota)
	}
	if !strings.EqualFold(classified.Model, "GLM-5.3-Trial") {
		t.Fatalf("model scope=%q want GLM-5.3-Trial", classified.Model)
	}

	classified, ok = zcodeEnvelopeError("GLM-5.3", http.StatusOK, body)
	if !ok {
		t.Fatalf("envelope not detected: %s", body)
	}
	if classified.Model != "" {
		t.Fatalf("a non-trial request must stay account-wide, got %q", classified.Model)
	}
}
