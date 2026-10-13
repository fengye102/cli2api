package zcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newClaimClient(t *testing.T, handler http.Handler, store *observeStore) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(store)
	client.http = server.Client()
	client.http.Transport = rewriteTransport{server: server.URL, base: client.http.Transport}
	client.catalogURL = server.URL
	return client
}

func claimOAuthStore() *observeStore {
	return &observeStore{memStore: memStore{items: map[string][]byte{
		"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"oauth","provider":"zai","zcode_jwt_token":"jwt-abc","email":"u@example.com"}`),
	}}}
}

func TestPreviewClaim_Plans(t *testing.T) {
	var sawPath, sawAuth, sawMid string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuth = r.Header.Get("Authorization")
		sawMid = r.Header.Get("X-Device-Mid")
		if r.URL.Path != "/api/v1/zcode-plan/billing/preview" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"plans":[
			{"plan_id":"zcode-v3-start-plan-trust-1002","name":"ZCode Trust Build","status":"active","starts_at":100,"ends_at":200}
		]}}`))
	})
	client := newClaimClient(t, handler, claimOAuthStore())
	plans, err := client.PreviewClaim(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("PreviewClaim error: %v", err)
	}
	if sawPath != "/api/v1/zcode-plan/billing/preview" {
		t.Errorf("path = %q", sawPath)
	}
	if sawAuth != "Bearer jwt-abc" {
		t.Errorf("Authorization = %q, want Bearer jwt-abc", sawAuth)
	}
	if sawMid == "" {
		t.Error("X-Device-Mid should be present for a known account")
	}
	if len(plans) != 1 || plans[0].PlanID != "zcode-v3-start-plan-trust-1002" {
		t.Fatalf("plans = %+v", plans)
	}
}

func TestPreviewClaim_NotDeployed(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	client := newClaimClient(t, handler, claimOAuthStore())
	plans, err := client.PreviewClaim(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("PreviewClaim on 404 should not error: %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("plans = %+v, want empty", plans)
	}
}

func TestClaim_Success(t *testing.T) {
	var sawPath, sawAuth, sawBody, sawMid, sawCaptcha string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuth = r.Header.Get("Authorization")
		sawMid = r.Header.Get("X-Device-Mid")
		sawCaptcha = r.Header.Get(CaptchaHeader)
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		sawBody = string(buf[:n])
		if r.URL.Path != "/api/v1/zcode-plan/billing/claim" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"plan":{"plan_id":"zcode-v3-start-plan-trust-1002","name":"ZCode Trust Build"}}}`))
	})
	client := newClaimClient(t, handler, claimOAuthStore())
	// captcha pool disabled by default (no node), so no captcha header expected.
	res, err := client.Claim(context.Background(), "acc1", "zcode-v3-start-plan-trust-1002")
	if err != nil {
		t.Fatalf("Claim error: %v", err)
	}
	if !res.Claimed {
		t.Fatalf("Claimed = false, want true: %+v", res)
	}
	if sawPath != "/api/v1/zcode-plan/billing/claim" {
		t.Errorf("path = %q", sawPath)
	}
	if sawAuth != "Bearer jwt-abc" {
		t.Errorf("Authorization = %q", sawAuth)
	}
	if sawMid == "" {
		t.Error("X-Device-Mid should be present on claim")
	}
	if !strings.Contains(sawBody, `"plan_id":"zcode-v3-start-plan-trust-1002"`) {
		t.Errorf("body = %q", sawBody)
	}
	if sawCaptcha != "" {
		t.Errorf("captcha header should be empty when pool disabled, got %q", sawCaptcha)
	}
}

func TestClaim_AlreadyClaimed(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1003,"msg":"already claimed","data":null}`))
	})
	client := newClaimClient(t, handler, claimOAuthStore())
	res, err := client.Claim(context.Background(), "acc1", "plan-x")
	if err != nil {
		t.Fatalf("Claim on 1003 should not hard-error: %v", err)
	}
	if !res.AlreadyClaimed {
		t.Fatalf("AlreadyClaimed = false, want true: %+v", res)
	}
}

func TestClaim_CaptchaRequired(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":3007,"msg":"captcha verify failed","data":null}`))
	})
	client := newClaimClient(t, handler, claimOAuthStore())
	_, err := client.Claim(context.Background(), "acc1", "plan-x")
	if err == nil {
		t.Fatal("Claim on 3007 should error")
	}
	if !strings.Contains(err.Error(), "captcha") {
		t.Errorf("error should mention captcha, got %q", err.Error())
	}
}

func TestClaim_NeedsAuth(t *testing.T) {
	store := &observeStore{memStore: memStore{items: map[string][]byte{
		"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"pat","provider":"zai","api_key":"key-abc"}`),
	}}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := newClaimClient(t, handler, store)
	_, err := client.Claim(context.Background(), "acc1", "plan-x")
	if err == nil {
		t.Fatal("Claim without OAuth JWT should error")
	}
	if !strings.Contains(err.Error(), "logged-in") {
		t.Errorf("error should mention login, got %q", err.Error())
	}
}
