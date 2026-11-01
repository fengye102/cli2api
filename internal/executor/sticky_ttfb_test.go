package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

func stickyPool(t *testing.T, ids ...string) *Pool {
	t.Helper()
	pool := NewPool(nil, nil)
	for _, id := range ids {
		pool.Upsert(Item{ID: id, URL: "http://" + id, Provider: "qoder", Region: "global", Runtime: "child_process"})
	}
	pool.SetRoutingStrategy(RoutingStrategyLowestTTFB)
	return pool
}

// A bound account that has genuinely gone slow must give the session up.
func TestStickyTooSlowEscapesWhenBoundAccountStalls(t *testing.T) {
	pool := stickyPool(t, "slow", "fast")
	seedTTFB(pool, "slow", 9000, 9000, 9000)
	seedTTFB(pool, "fast", 300, 300, 300)
	if !pool.StickyTooSlow("slow", "glm-5.2") {
		t.Fatal("a 9s bound account with a 300ms alternative must escape")
	}
}

// A small gap is not worth churning a session over: staying put keeps cache
// locality and stable behaviour.
func TestStickyTooSlowKeepsCloseCall(t *testing.T) {
	pool := stickyPool(t, "slow", "fast")
	seedTTFB(pool, "slow", 2500, 2500, 2500)
	seedTTFB(pool, "fast", 1200, 1200, 1200)
	if pool.StickyTooSlow("slow", "glm-5.2") {
		t.Fatal("a 1.3s gap must not break a session binding")
	}
}

// Unknown is not slow. Without samples on both sides the escape would be a
// guess, and would move sessions on the first request of every new account.
func TestStickyTooSlowNeedsSamplesOnBothSides(t *testing.T) {
	pool := stickyPool(t, "slow", "fast")
	seedTTFB(pool, "fast", 300, 300, 300)
	if pool.StickyTooSlow("slow", "glm-5.2") {
		t.Fatal("escaped on an unsampled bound account")
	}
	pool = stickyPool(t, "slow", "fast")
	seedTTFB(pool, "slow", 9000, 9000, 9000)
	if pool.StickyTooSlow("slow", "glm-5.2") {
		t.Fatal("escaped towards an account with no sample")
	}
}

// The escape must not send a request at an account the picker would reject:
// trading a slow answer for no answer is worse than staying.
func TestStickyTooSlowIgnoresCoolingAlternatives(t *testing.T) {
	pool := stickyPool(t, "slow", "fast")
	seedTTFB(pool, "slow", 9000, 9000, 9000)
	seedTTFB(pool, "fast", 300, 300, 300)
	pool.MarkDown("fast", time.Hour, "cooling")
	if pool.StickyTooSlow("slow", "glm-5.2") {
		t.Fatal("escaped towards a cooling account")
	}
}

// Every other strategy keeps its old behaviour: latency samples may exist, but
// they must not move sessions unless the operator opted into latency routing.
func TestStickyTooSlowIsGatedOnTheStrategy(t *testing.T) {
	pool := stickyPool(t, "slow", "fast")
	seedTTFB(pool, "slow", 9000, 9000, 9000)
	seedTTFB(pool, "fast", 300, 300, 300)
	pool.SetRoutingStrategy(RoutingStrategyRoundRobin)
	if pool.StickyTooSlow("slow", "glm-5.2") {
		t.Fatal("round-robin must never escape a binding on latency")
	}
}

// End to end: the session moves to the account that actually answers fast, and
// the binding follows it, so the next request does not have to escape again.
func TestChatNonStreamStickyEscapesSlowFirstByte(t *testing.T) {
	server := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"model":"glm-5.2","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"source":"upstream"}}`)
		}))
	}
	slow := server()
	defer slow.Close()
	fast := server()
	defer fast.Close()

	pool := NewPool(nil, nil)
	pool.Upsert(Item{ID: "slow", URL: slow.URL, Provider: "qoder", Region: "global", Runtime: "child_process"})
	pool.Upsert(Item{ID: "fast", URL: fast.URL, Provider: "qoder", Region: "global", Runtime: "child_process"})
	pool.SetRoutingStrategy(RoutingStrategyLowestTTFB)
	executor := NewChatExecutor(pool, "")
	executor.SessionAffinity.Bind("session-1", "slow")
	seedTTFB(pool, "slow", 9000, 9000, 9000)
	seedTTFB(pool, "fast", 300, 300, 300)

	result, err := executor.ChatNonStream(WithSessionKey(context.Background(), "session-1"), translate.ChatRequest{
		Model: "glm-5.2", Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	}, "", "")
	if err != nil || result.AccountID != "fast" || result.Routing != routingStickyEscape {
		t.Fatalf("result = %+v, err=%v; want the fast account via sticky_escape", result, err)
	}
	if bound, ok := executor.SessionAffinity.Get("session-1"); !ok || bound != "fast" {
		t.Fatalf("rebound session = %q, %v; want the fast account", bound, ok)
	}
	if reason := executor.SessionAffinity.Stats().LastEscapeReason; reason != "slow_first_byte" {
		t.Fatalf("escape reason = %q, want slow_first_byte", reason)
	}
}

// The same request on a healthy binding must stay put: this is the guard that
// keeps the escape from turning into "always pick the global fastest account",
// which would defeat the point of session affinity.
func TestChatNonStreamStickyKeepsHealthyBinding(t *testing.T) {
	server := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"model":"glm-5.2","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"source":"upstream"}}`)
		}))
	}
	bound := server()
	defer bound.Close()
	other := server()
	defer other.Close()

	pool := NewPool(nil, nil)
	pool.Upsert(Item{ID: "bound", URL: bound.URL, Provider: "qoder", Region: "global", Runtime: "child_process"})
	pool.Upsert(Item{ID: "other", URL: other.URL, Provider: "qoder", Region: "global", Runtime: "child_process"})
	pool.SetRoutingStrategy(RoutingStrategyLowestTTFB)
	executor := NewChatExecutor(pool, "")
	executor.SessionAffinity.Bind("session-1", "bound")
	seedTTFB(pool, "bound", 900, 900, 900)
	seedTTFB(pool, "other", 300, 300, 300)

	result, err := executor.ChatNonStream(WithSessionKey(context.Background(), "session-1"), translate.ChatRequest{
		Model: "glm-5.2", Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	}, "", "")
	if err != nil || result.AccountID != "bound" || result.Routing != routingSticky {
		t.Fatalf("result = %+v, err=%v; want the bound account kept", result, err)
	}
}
