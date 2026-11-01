package executor

import (
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// TestClassifyErrorKeepsTheProviderModelScope checks the provider's narrowing
// survives the trip into the executor: a quota refusal about one model must not
// come back account-wide.
func TestClassifyErrorKeepsTheProviderModelScope(t *testing.T) {
	scoped := ClassifyError(&providers.Error{
		Kind:    accounts.KindQuota,
		Status:  400,
		Message: "ZCode has no active limited quota for GLM-5.3-Trial",
		Model:   "GLM-5.3-Trial",
	})
	if scoped.Kind != accounts.KindQuota {
		t.Fatalf("kind=%q want %q", scoped.Kind, accounts.KindQuota)
	}
	if scoped.Model != "GLM-5.3-Trial" {
		t.Fatalf("model scope=%q, want GLM-5.3-Trial", scoped.Model)
	}

	accountWide := ClassifyError(&providers.Error{
		Kind:    accounts.KindQuota,
		Status:  429,
		Message: "insufficient balance",
	})
	if accountWide.Model != "" {
		t.Fatalf("a provider that names no model must stay account-wide, got %q", accountWide.Model)
	}
}

// TestQuotaCooldownRespectsTheModelScope pins the pool behaviour both ways: a
// model-scoped quota rests that model alone and leaves the account routable,
// while an account-wide quota still rests everything.
func TestQuotaCooldownRespectsTheModelScope(t *testing.T) {
	pool := NewPool([]string{"http://127.0.0.1:1"}, []string{"acc"})
	pool.Upsert(Item{ID: "acc"})

	pool.MarkClassified("acc", Classified{
		Kind:     accounts.KindQuota,
		Status:   400,
		Message:  "ZCode has no active limited quota for GLM-5.3-Trial",
		Model:    "GLM-5.3-Trial",
		Cooldown: time.Hour,
	})

	item, ok := pool.ByID("acc")
	if !ok {
		t.Fatal("account missing")
	}
	if !item.DownUntil.IsZero() {
		t.Fatalf("a model-scoped quota must not park the account: %s", item.DownUntil)
	}
	if until, ok := item.ModelDownUntil["glm-5.3-trial"]; !ok || until.IsZero() {
		t.Fatalf("GLM-5.3-Trial must be rested, got %v", item.ModelDownUntil)
	}
	if _, ok := item.ModelDownUntil["glm-5.3"]; ok {
		t.Fatalf("the paid model must keep serving: %v", item.ModelDownUntil)
	}

	// Account-wide quota: no model named, so the whole account rests.
	pool.MarkClassified("acc", Classified{
		Kind:     accounts.KindQuota,
		Status:   429,
		Message:  "insufficient balance",
		Cooldown: time.Hour,
	})
	item, _ = pool.ByID("acc")
	if item.DownUntil.IsZero() {
		t.Fatal("an account-wide quota must still rest the account")
	}
}
