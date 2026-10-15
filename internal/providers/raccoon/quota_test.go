package raccoon

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// balanceBody renders the balance endpoint's response shape: the {code, message,
// data} envelope with available_points plus the four wallet buckets.
func balanceBody(available string, wallets ...[2]string) string {
	fields := []string{`"available_points":` + available}
	for _, wallet := range wallets {
		fields = append(fields, `"`+wallet[0]+`":`+wallet[1])
	}
	return `{"code":0,"data":{` + strings.Join(fields, ",") + `}}`
}

// quotaClient wires a Client whose balance and subscription endpoints are served
// by balanceHandler / entitlementHandler. A nil entitlementHandler answers with
// an empty subscription, so a test that only cares about the balance does not
// have to spell out the second call Quota always makes.
func quotaClient(t *testing.T, balanceHandler, entitlementHandler http.HandlerFunc) (*Client, *memStore) {
	t.Helper()
	if entitlementHandler == nil {
		entitlementHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
		}
	}
	return checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, balancePath):
			balanceHandler(w, r)
		case strings.HasSuffix(r.URL.Path, entitlementPath):
			entitlementHandler(w, r)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})
}

// TestQuotaReadsBalanceAndWallets is the happy path: the balance endpoint
// answers with a total plus wallet buckets, and every field reaches the snapshot.
func TestQuotaReadsBalanceAndWallets(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Errorf("balance auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, balanceBody("1250",
			[2]string{"daily_points", "50"},
			[2]string{"monthly_points", "1000"},
			[2]string{"reward_points", "200"},
			[2]string{"topup_points", "0"},
		))
	}, nil)

	info, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if info.Remaining != 1250 || info.Total != 1250 {
		t.Fatalf("total/remaining = %v/%v, want 1250", info.Total, info.Remaining)
	}
	if info.Unit != "积分" {
		t.Fatalf("unit = %q", info.Unit)
	}
	if info.ProviderID != "raccoon" {
		t.Fatalf("provider = %q", info.ProviderID)
	}
	// available + four wallets
	if len(info.Windows) != 5 {
		t.Fatalf("windows = %+v, want 5", info.Windows)
	}
	if info.Windows[0].ID != "available" || info.Windows[0].Remaining != 1250 {
		t.Fatalf("windows[0] = %+v", info.Windows[0])
	}
	labels := []string{"每日积分", "每月会员积分", "奖励积分", "充值积分"}
	for i, label := range labels {
		if got := info.Windows[i+1].Label; got != label {
			t.Errorf("windows[%d].Label = %q, want %q", i+1, got, label)
		}
	}
	// A zero wallet is still reported: it is a real bucket, not a missing field.
	if info.Windows[4].Remaining != 0 {
		t.Errorf("topup window = %+v, want 0", info.Windows[4])
	}
}

// TestQuotaZeroBalanceStillReportsWindow pins the reason the "available" window
// is always emitted: a window-less snapshot is treated as "not fetched yet" by
// the runtime (quotaInfoHasWindows) and dropped, so an empty account would show
// "no quota data" forever instead of an honest zero.
func TestQuotaZeroBalanceStillReportsWindow(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, balanceBody("0"))
	}, nil)
	info, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if len(info.Windows) != 1 {
		t.Fatalf("windows = %+v, want the available window", info.Windows)
	}
	if info.Remaining != 0 || info.Windows[0].Remaining != 0 {
		t.Fatalf("zero balance = %+v", info)
	}
	// A zero balance must not be reported as exhausted: routing uses Exceeded
	// to exclude the account, and free models still serve at zero points.
	if info.Exceeded {
		t.Error("zero balance should not set Exceeded")
	}
}

// TestQuotaStringNumbers covers upstream writing credit fields as strings.
func TestQuotaStringNumbers(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, balanceBody(`"800"`, [2]string{"daily_points", `"12"`}))
	}, nil)
	info, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if info.Remaining != 800 {
		t.Fatalf("remaining = %v, want 800", info.Remaining)
	}
	if len(info.Windows) != 2 || info.Windows[1].Remaining != 12 {
		t.Fatalf("windows = %+v", info.Windows)
	}
}

// TestQuotaMissingAvailablePointsFails: a response without the field is not a
// zero balance, so it must surface as an error and leave the stored snapshot
// alone rather than overwriting it with a fabricated 0.
func TestQuotaMissingAvailablePointsFails(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"daily_points":5}}`)
	}, nil)
	if _, err := client.Quota(context.Background(), "acc-1"); err == nil {
		t.Fatal("missing available_points should be an error")
	}
}

// TestQuotaUpstreamCodeFails: a non-zero business code is a failure, not a
// silent zero. This is the shape upstream returns when the token is rejected.
func TestQuotaUpstreamCodeFails(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":200001,"message":"authorization_empty_error"}`)
	}, nil)
	info, err := client.Quota(context.Background(), "acc-1")
	if err == nil {
		t.Fatalf("business code should fail, got %+v", info)
	}
	if !strings.Contains(err.Error(), "authorization_empty_error") {
		t.Fatalf("error should carry the upstream message: %v", err)
	}
}

// TestQuotaRefreshesOn401 is the token-expiry path: a stale stored access token
// must be refreshed once and the read retried, the same way chat and check-in do.
func TestQuotaRefreshesOn401(t *testing.T) {
	var balanceCalls, refreshCalls int
	client, store := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		balanceCalls++
		if balanceCalls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":200001,"message":"authorization_empty_error"}`)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer new-tok" {
			t.Errorf("retry auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, balanceBody("42"))
	}, nil)
	// A refreshable credential: the 401 path mints a new access token from it.
	payload, err := EncodeCredential(Credential{Format: CredentialFormat, AccessToken: "token-1", RefreshToken: "ref-1"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	store.items["acc-1"] = payload
	// The auth base points at the same stub; serve the refresh endpoint there.
	client.authBase = client.siteBase
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, refreshPath) {
			refreshCalls++
			return jsonResponse(r, `{"access_token":"new-tok","refresh_token":"ref-2"}`), nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})}

	info, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if info.Remaining != 42 {
		t.Fatalf("remaining = %v, want 42", info.Remaining)
	}
	if balanceCalls != 2 {
		t.Errorf("balance calls = %d, want 2", balanceCalls)
	}
	if refreshCalls != 1 {
		t.Errorf("refresh calls = %d, want 1", refreshCalls)
	}
	if saved := string(store.items["acc-1"]); !strings.Contains(saved, "new-tok") {
		t.Errorf("rotated token not persisted: %q", saved)
	}
}

// TestQuotaSubscriptionIsBestEffort: a subscription failure must not fail the
// reading. The balance is the authority; the plan name is additional detail.
func TestQuotaSubscriptionIsBestEffort(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, balanceBody("777"))
	}, func(w http.ResponseWriter, r *http.Request) {
		// Upstream hiccup on the subscription call.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})

	info, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("subscription failure must not fail the reading: %v", err)
	}
	if info.Remaining != 777 {
		t.Fatalf("remaining = %v, want 777", info.Remaining)
	}
	if info.Plan != "" {
		t.Fatalf("plan = %q, want empty on subscription failure", info.Plan)
	}
}

// TestQuotaReadsSubscriptionPlan covers the entitlement shape: office wins over
// code, and both spellings of the tier field resolve.
func TestQuotaReadsSubscriptionPlan(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, balanceBody("500"))
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{
			"office":{"active_plan":{"type":"raccoon-pro"}},
			"code":{"active_plan":{"type":"raccoon-code"}}
		}}`)
	})

	info, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if info.Plan != "raccoon-pro" {
		t.Fatalf("plan = %q, want office tier to win", info.Plan)
	}
}

// TestQuotaSubscriptionFallsBackToCode: with no office set, the code set's
// plan_type spelling still resolves.
func TestQuotaSubscriptionFallsBackToCode(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, balanceBody("500"))
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"code":{"plan_type":"code-tier"}}}`)
	})

	info, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if info.Plan != "code-tier" {
		t.Fatalf("plan = %q, want code-tier", info.Plan)
	}
}

// TestQuotaCredentialMintFromRefreshToken covers a credential that stores only a
// refresh token: the token must be minted before the balance call.
func TestQuotaCredentialMintFromRefreshToken(t *testing.T) {
	var refreshCalls int
	client, store := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer minted-tok" {
			t.Errorf("balance auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, balanceBody("9"))
	}, nil)
	payload, err := EncodeCredential(Credential{Format: CredentialFormat, RefreshToken: "ref-only"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	store.items["acc-1"] = payload
	client.authBase = client.siteBase
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, refreshPath) {
			refreshCalls++
			return jsonResponse(r, `{"access_token":"minted-tok"}`), nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})}

	info, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if info.Remaining != 9 {
		t.Fatalf("remaining = %v, want 9", info.Remaining)
	}
	if refreshCalls != 1 {
		t.Errorf("refresh calls = %d, want 1", refreshCalls)
	}
}

// TestQuotaUnparsablePayloadFails guards against a non-object body.
func TestQuotaUnparsablePayloadFails(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `"not-an-object"`)
	}, nil)
	if _, err := client.Quota(context.Background(), "acc-1"); err == nil {
		t.Fatal("non-object payload should be an error")
	}
}

// TestPointsValue covers the field reader directly: numbers, string numbers,
// null, missing and garbage.
func TestPointsValue(t *testing.T) {
	cases := []struct {
		raw   string
		want  float64
		found bool
	}{
		{`1250`, 1250, true},
		{`12.7`, 12, true}, // credits are integral upstream; floor like the reference
		{`"800"`, 800, true},
		{`0`, 0, true},
		{`null`, 0, false},
		{``, 0, false},
		{`"abc"`, 0, false},
		{`{}`, 0, false},
	}
	for _, c := range cases {
		got, found := pointsValue([]byte(c.raw))
		if found != c.found || (found && got != c.want) {
			t.Errorf("pointsValue(%q) = %v/%v, want %v/%v", c.raw, got, found, c.want, c.found)
		}
	}
}

// TestQuotaInfoSurvivesWindowGuard pins the contract with the runtime: the
// snapshot this adapter returns must pass quotaInfoHasWindows, otherwise the
// reading is silently dropped and the card keeps saying "no quota data".
func TestQuotaInfoSurvivesWindowGuard(t *testing.T) {
	client, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, balanceBody("0"))
	}, nil)
	info, err := client.Quota(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	var _ *providers.QuotaInfo = info
	if len(info.Windows) == 0 && !(info.Total > 0 || info.Remaining > 0) {
		t.Fatal("snapshot would be dropped by the runtime's window guard")
	}
}

// roundTripFunc adapts a function to http.RoundTripper so a test can intercept
// one endpoint (the token refresh) while the rest go to the real stub server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// jsonResponse builds a 200 JSON response for an intercepted request.
func jsonResponse(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}
