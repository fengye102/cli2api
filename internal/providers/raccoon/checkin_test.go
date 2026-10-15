package raccoon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// checkinClient wires a Client to a stub main site. The auth/LLM bases point at
// the same server so credential refresh also works against the stub.
func checkinClient(t *testing.T, handler http.HandlerFunc) (*Client, *memStore) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	store := &memStore{items: map[string][]byte{}}
	payload, err := EncodeCredential(Credential{Format: CredentialFormat, AccessToken: "token-1"})
	if err != nil {
		t.Fatalf("encode credential: %v", err)
	}
	store.items["acc-1"] = payload
	client := NewClient(store)
	client.siteBase = server.URL
	client.authBase = server.URL
	client.llmBase = server.URL
	return client, store
}

// billsBody renders a bills response with one row per (points, createdAt) pair.
func billsBody(rows ...[2]string) string {
	items := make([]string, 0, len(rows))
	for _, row := range rows {
		items = append(items, `{"points":`+row[0]+`,"created_at":"`+row[1]+`","event_name":"每日登录积分"}`)
	}
	return `{"code":0,"data":{"items":[` + strings.Join(items, ",") + `]}}`
}

// todayAt renders a Beijing-time timestamp for today at the given clock time.
func todayAt(clock string) string {
	return time.Now().In(beijingZone).Format("2006-01-02") + "T" + clock + ":00+08:00"
}

// TestCheckinReportsSuccessWhenTodaysCreditLands is the happy path: the grant
// trigger fires and today's credit is visible in the ledger.
func TestCheckinReportsSuccessWhenTodaysCreditLands(t *testing.T) {
	var sawSettingInfo bool
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, settingInfoPath):
			sawSettingInfo = true
			// The desktop identity headers must be present: the upstream
			// settles the daily grant on the desktop branch.
			if got := r.Header.Get("X-Client-Platform"); got != "desktop-windows" {
				t.Errorf("X-Client-Platform = %q", got)
			}
			if got := r.Header.Get("X-Client-Version"); got != "v1.0.0" {
				t.Errorf("X-Client-Version = %q", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
				t.Errorf("Authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"point_grant_popups":[],"point_grant_toast":"今日登录积分 +100"}}`))
		case strings.HasSuffix(r.URL.Path, billsPath):
			_, _ = w.Write([]byte(billsBody([2]string{"100", todayAt("09:30")})))
		default:
			http.NotFound(w, r)
		}
	})
	result, err := client.Checkin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !sawSettingInfo {
		t.Fatal("the grant trigger must be called")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q", result.Status)
	}
	if result.RewardCredits != 100 {
		t.Fatalf("reward = %v", result.RewardCredits)
	}
	if !strings.Contains(result.Message, "+100") {
		t.Fatalf("message should report the credited points: %q", result.Message)
	}
}

// TestCheckinSumsMultipleTodaysRows covers a day with more than one grant row.
func TestCheckinSumsMultipleTodaysRows(t *testing.T) {
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, settingInfoPath) {
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		_, _ = w.Write([]byte(billsBody(
			[2]string{"100", todayAt("09:30")},
			[2]string{"50", todayAt("14:00")},
		)))
	})
	result, err := client.Checkin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.RewardCredits != 150 {
		t.Fatalf("reward = %v, want 150", result.RewardCredits)
	}
}

// TestCheckinIgnoresYesterdaysAndNonPositiveRows is the day-boundary assertion:
// only today's positive rows may count, or "today's credit" would be satisfied
// by an old grant and the check-in would silently never run again.
func TestCheckinIgnoresYesterdaysAndNonPositiveRows(t *testing.T) {
	yesterday := time.Now().In(beijingZone).AddDate(0, 0, -1).Format("2006-01-02") + "T09:30:00+08:00"
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, settingInfoPath) {
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		_, _ = w.Write([]byte(billsBody(
			[2]string{"999", yesterday},
			[2]string{"0", todayAt("09:30")},
			[2]string{"-5", todayAt("10:00")},
		)))
	})
	if _, err := client.Checkin(context.Background(), "acc-1"); err == nil {
		t.Fatal("no credit landed today, so the check-in must not report success")
	}
}

// TestCheckinFailsWhenGrantTriggerFails pins that a failed trigger is a real
// failure rather than a silent success.
func TestCheckinFailsWhenGrantTriggerFails(t *testing.T) {
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, settingInfoPath) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(billsBody([2]string{"100", todayAt("09:30")})))
	})
	if _, err := client.Checkin(context.Background(), "acc-1"); err == nil {
		t.Fatal("a failed grant trigger must surface as an error")
	}
}

// TestCheckinFailsWhenLedgerUnavailable covers the honesty rule: without the
// ledger there is no evidence the credit landed, so success must not be claimed
// even though the trigger itself succeeded.
func TestCheckinFailsWhenLedgerUnavailable(t *testing.T) {
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, settingInfoPath) {
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := client.Checkin(context.Background(), "acc-1"); err == nil {
		t.Fatal("an unverifiable check-in must not report success")
	}
}

// TestCheckinRefreshesAndRetriesOnExpiredAccessToken covers the stale-token
// path: the stored access token is refused, the check-in mints a new one from
// the refresh token, and the retry succeeds. Without this an aged-out account
// would report a permanent failure until a human re-logged in.
func TestCheckinRefreshesAndRetriesOnExpiredAccessToken(t *testing.T) {
	var refreshCalls, settingCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/refresh"):
			refreshCalls++
			_, _ = w.Write([]byte(`{"access_token":"fresh-1","refresh_token":"refresh-1"}`))
		case strings.HasSuffix(r.URL.Path, settingInfoPath):
			settingCalls++
			if r.Header.Get("Authorization") == "Bearer stale-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer fresh-1" {
				t.Errorf("retry Authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, billsPath):
			_, _ = w.Write([]byte(billsBody([2]string{"120", todayAt("09:30")})))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := &memStore{items: map[string][]byte{}}
	payload, err := EncodeCredential(Credential{Format: CredentialFormat, AccessToken: "stale-1", RefreshToken: "refresh-1"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	store.items["acc-1"] = payload
	client := NewClient(store)
	client.siteBase = server.URL
	client.authBase = server.URL
	result, err := client.Checkin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if result.Status != "success" || result.RewardCredits != 120 {
		t.Fatalf("result = %+v", result)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want exactly one", refreshCalls)
	}
	if settingCalls != 2 {
		t.Fatalf("grant trigger calls = %d, want 2 (refused + retry)", settingCalls)
	}
}

// TestCheckinReportsExpiredSession covers the 401 branch: the operator must be
// told to log in again rather than seeing a generic transport failure. With no
// refresh token there is nothing to retry with, so the 401 must surface.
func TestCheckinReportsExpiredSession(t *testing.T) {
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := client.Checkin(context.Background(), "acc-1")
	if err == nil {
		t.Fatal("a 401 must fail")
	}
	if !strings.Contains(err.Error(), "登录态已过期") {
		t.Fatalf("401 message should say the session expired: %v", err)
	}
}

// TestCheckinSurfacesUpstreamBusinessCode covers the {code != 0} envelope: the
// upstream message must reach the operator.
func TestCheckinSurfacesUpstreamBusinessCode(t *testing.T) {
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, settingInfoPath) {
			_, _ = w.Write([]byte(`{"code":40001,"message":"积分功能维护中"}`))
			return
		}
		_, _ = w.Write([]byte(billsBody([2]string{"100", todayAt("09:30")})))
	})
	_, err := client.Checkin(context.Background(), "acc-1")
	if err == nil {
		t.Fatal("a non-zero business code must fail")
	}
	if !strings.Contains(err.Error(), "积分功能维护中") {
		t.Fatalf("upstream message must be surfaced: %v", err)
	}
}

// TestCheckinMintsTokenFromRefreshTokenOnly covers an imported account that
// holds only a refresh token: the check-in must mint an access token first
// rather than failing with "no credential".
func TestCheckinMintsTokenFromRefreshTokenOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/refresh"):
			// The refresh endpoint answers with a flat token pair.
			_, _ = w.Write([]byte(`{"access_token":"minted-1","refresh_token":"refresh-1"}`))
		case strings.HasSuffix(r.URL.Path, settingInfoPath):
			if got := r.Header.Get("Authorization"); got != "Bearer minted-1" {
				t.Errorf("Authorization = %q, want the minted token", got)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, billsPath):
			_, _ = w.Write([]byte(billsBody([2]string{"80", todayAt("09:30")})))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := &memStore{items: map[string][]byte{}}
	payload, err := EncodeCredential(Credential{Format: CredentialFormat, RefreshToken: "refresh-1"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	store.items["acc-1"] = payload
	client := NewClient(store)
	client.siteBase = server.URL
	client.authBase = server.URL
	result, err := client.Checkin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Checkin with a refresh-token-only credential: %v", err)
	}
	if result.Status != "success" || result.RewardCredits != 80 {
		t.Fatalf("result = %+v", result)
	}
}

// TestCheckinFailsWithoutAnyCredential guards against inventing a credential.
func TestCheckinFailsWithoutAnyCredential(t *testing.T) {
	client, store := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {})
	payload, err := EncodeCredential(Credential{Format: CredentialFormat})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	store.items["acc-1"] = payload
	if _, err := client.Checkin(context.Background(), "acc-1"); err == nil {
		t.Fatal("a credential with no token must fail")
	}
}

// TestAdapterExposesCheckin pins the capability wiring the scheduler reads: a
// nil Checkin would silently skip every account of this provider.
func TestAdapterExposesCheckin(t *testing.T) {
	client, _ := checkinClient(t, func(w http.ResponseWriter, r *http.Request) {})
	adapter := client.Adapter()
	if adapter.Checkin == nil {
		t.Fatal("raccoon must expose a check-in implementation")
	}
	if _, ok := adapter.Checkin.(providers.AccountCheckiner); !ok {
		t.Fatal("the check-in implementation must satisfy providers.AccountCheckiner")
	}
}

// TestGrantNoteRendersToast covers the notification passthrough.
func TestGrantNoteRendersToast(t *testing.T) {
	if note := grantNote(json.RawMessage(`{"point_grant_toast":"今日登录积分 +100"}`)); note != "今日登录积分 +100" {
		t.Fatalf("note = %q", note)
	}
	if note := grantNote(json.RawMessage(`{"point_grant_popups":[]}`)); note != "" {
		t.Fatalf("an empty popups list must render nothing, got %q", note)
	}
	if note := grantNote(nil); note != "" {
		t.Fatalf("nil grant must render nothing, got %q", note)
	}
}
