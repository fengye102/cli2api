package executor

import (
	"context"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// The routing affinity and the providers that keep an upstream prompt cache
// warm both need the conversation. Providers cannot import this package, so
// WithSessionKey hands it over through the shared provider context: a provider
// that reads it and one that does not can both live with a single ctx.
func TestWithSessionKeyHandsTheConversationToProviders(t *testing.T) {
	ctx := WithSessionKey(context.Background(), " conversation-42 ")
	if got := sessionKeyFromContext(ctx); got != "conversation-42" {
		t.Errorf("routing key=%q", got)
	}
	if got := providers.SessionHintFromContext(ctx); got != "conversation-42" {
		t.Errorf("provider hint=%q want the same conversation", got)
	}

	// An unnamed conversation stays unnamed, rather than becoming "".
	if got := providers.SessionHintFromContext(WithSessionKey(context.Background(), "   ")); got != "" {
		t.Errorf("blank key leaked into the provider context: %q", got)
	}
	if got := providers.SessionHintFromContext(context.Background()); got != "" {
		t.Errorf("bare context carries a hint: %q", got)
	}
}
