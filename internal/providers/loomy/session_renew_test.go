package loomy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Remembering the phone number is what makes a lapsed session cheap: an
// operator whose 14-day session expired should get away with one click and six
// digits, not a retyped number.
func TestStartLoginReusesStoredPhone(t *testing.T) {
	store := newFakeStore()
	gateway := newFakeGateway(t, "sess-1")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)
	store.add("acc-1", Credential{
		Format:    CredentialFormat,
		LoginID:   "13800138000",
		Password:  "pw",
		Session:   "sess-1",
		ExpiresAt: farFuture(),
	})

	flow := loginFlow{client: client}
	session, err := flow.StartLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if session.State != "sms" {
		t.Fatalf("state = %q", session.State)
	}
	if _, message, err := flow.PollLogin(context.Background(), "acc-1"); err != nil {
		t.Fatalf("PollLogin: %v", err)
	} else if !strings.Contains(message, "138****8000") {
		t.Fatalf("the round should have texted the remembered number, poll = %q", message)
	}
}

// A brand new account has nothing remembered yet, so the round must still wait
// for the operator to paste the number.
func TestStartLoginWithoutStoredPhoneWaitsForPaste(t *testing.T) {
	store := newFakeStore()
	gateway := newFakeGateway(t, "sess-1")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)
	store.addAccount("acc-1")

	flow := loginFlow{client: client}
	if _, err := flow.StartLogin(context.Background(), "acc-1"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	_, message, err := flow.PollLogin(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if !strings.Contains(message, "手机号") {
		t.Fatalf("poll without a phone should ask for one, got %q", message)
	}
}

func TestRenewSessionLeavesFreshSessionsAlone(t *testing.T) {
	store := newFakeStore()
	gateway := newFakeGateway(t, "sess-new")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)
	store.add("acc-1", Credential{
		Format:    CredentialFormat,
		LoginID:   "13800138000",
		Password:  "pw",
		Session:   "sess-keep",
		ExpiresAt: time.Now().Add(10 * 24 * time.Hour).Unix(),
	})

	if err := client.RenewSession(context.Background(), "acc-1"); err != nil {
		t.Fatalf("RenewSession: %v", err)
	}
	if cred := store.credential(t, "acc-1"); cred.Session != "sess-keep" {
		t.Fatalf("a session with 10 days left must not be replaced, got %q", cred.Session)
	}
}

func TestRenewSessionRefreshesNearExpiry(t *testing.T) {
	store := newFakeStore()
	gateway := newFakeGateway(t, "sess-new")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)
	store.add("acc-1", Credential{
		Format:    CredentialFormat,
		LoginID:   "13800138000",
		Password:  "pw",
		Session:   "sess-old",
		ExpiresAt: time.Now().Add(6 * time.Hour).Unix(),
	})

	if err := client.RenewSession(context.Background(), "acc-1"); err != nil {
		t.Fatalf("RenewSession: %v", err)
	}
	cred := store.credential(t, "acc-1")
	if cred.Session == "sess-old" || !strings.HasPrefix(cred.Session, "sess-") {
		t.Fatalf("session = %q, want the freshly minted one", cred.Session)
	}
	if cred.ExpiresAt < time.Now().Add(13*24*time.Hour).Unix() {
		t.Fatalf("expiry = %v, want a full new session window", time.Unix(cred.ExpiresAt, 0))
	}
}

// An imported session or an SMS-only account has no password: renewal must stay
// silent instead of hammering the account service every hour.
func TestRenewSessionSkipsAccountsWithoutPassword(t *testing.T) {
	store := newFakeStore()
	gateway := newFakeGateway(t, "sess-new")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)
	store.add("acc-1", Credential{
		Format:    CredentialFormat,
		LoginID:   "13800138000",
		Session:   "sess-pasted",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	})

	if err := client.RenewSession(context.Background(), "acc-1"); err != nil {
		t.Fatalf("RenewSession: %v", err)
	}
	if cred := store.credential(t, "acc-1"); cred.Session != "sess-pasted" {
		t.Fatalf("session = %q, want it untouched", cred.Session)
	}
}

// The maintenance loop walks every account, including brand new rows that have
// no credential yet; renewal must treat that as "nothing to do".
func TestRenewSessionIgnoresAccountWithoutCredential(t *testing.T) {
	store := newFakeStore()
	gateway := newFakeGateway(t, "sess-new")
	account := newFakeAccountService(t, "pw")
	client := newTestClient(t, store, gateway, account)
	store.addAccount("acc-1")

	if err := client.RenewSession(context.Background(), "acc-1"); err != nil {
		t.Fatalf("RenewSession on a row without a credential: %v", err)
	}
}
