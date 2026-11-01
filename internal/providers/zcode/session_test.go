package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// The vendor's prompt cache follows the conversation, not the device: magpie's
// plugins#387 measured a cache hit 1 time in 19 for a coding plan's requests
// that named no session, and ZCode's own client never sends one empty. These
// tests pin the shape (a bare lowercase UUID, the same on every turn) and the
// two places it goes (metadata.user_id for both credential modes).

func TestSessionOf(t *testing.T) {
	const device = "0b6f1a5e-1111-4222-8333-444455556666"
	uuidHint := "6B1F8F3C-1111-4222-8333-444455556666"

	// A caller that already names its conversation in the client's format goes
	// as it is, lowercased: the same value on both sides is what keeps the
	// cache warm when the client reconnects.
	if got := sessionOf(uuidHint, device); got != "6b1f8f3c-1111-4222-8333-444455556666" {
		t.Errorf("UUID hint: %q want the bare lowercased UUID", got)
	}

	// The gateway's own session key is a hex digest; it is hashed into the
	// client's shape, deterministically.
	hexHint := "9f2c1d3e4b5a69788796a5b4c3d2e1f0091827364554637281900a1b2c3d4e5f"
	first := sessionOf(hexHint, device)
	if !uuidShape.MatchString(first) {
		t.Fatalf("hex hint: %q is not a UUID", first)
	}
	if first != sessionOf(hexHint, device) {
		t.Error("the same conversation must render the same session id on every turn")
	}
	if first == sessionOf(hexHint+"0", device) {
		t.Error("two conversations must not share a session id")
	}
	if first == device {
		t.Error("session id must not be the device id: the vendor keeps them apart")
	}

	// Whitespace never changes the conversation.
	if sessionOf("  "+hexHint+"  ", device) != first {
		t.Error("surrounding whitespace must not fork the session")
	}

	// No conversation at all: fall back to the device, which at least keeps one
	// process's turns together.
	byDevice := sessionOf("", device)
	if !uuidShape.MatchString(byDevice) || byDevice == first {
		t.Errorf("device fallback: %q", byDevice)
	}
	if byDevice != sessionOf("", device) {
		t.Error("device fallback must be stable")
	}
	// Nothing to name: empty, so the caller can leave the field out entirely.
	if got := sessionOf("", ""); got != "" {
		t.Errorf("no device, no conversation: %q want empty", got)
	}
}

func TestMarkSessionKeepsTheCallersOwnIdentity(t *testing.T) {
	callers := `{"device_id":"theirs","account_uuid":"","session_id":"keep-me"}`
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"metadata": map[string]any{"user_id": callers},
	}
	if markSession(body, "0b6f1a5e-1111-4222-8333-444455556666", "session-1") {
		t.Error("a caller that named its own device keeps it")
	}
	if got := body["metadata"].(map[string]any)["user_id"]; got != callers {
		t.Errorf("metadata.user_id=%v want the caller's %s", got, callers)
	}

	// A body without a messages array is not a Messages request: leave it be.
	if markSession(map[string]any{"input": "x"}, "dev", "session-1") {
		t.Error("only Messages requests carry the field")
	}
	// Nothing to name without a device.
	if markSession(map[string]any{"messages": []any{}}, "", "session-1") {
		t.Error("a request with no device must stay unnamed")
	}
}

// TestCodingPlanRequestNamesTheSession is the bug this change fixes: an api-key
// credential talks to the coding-plan endpoint, where the vendor's prompt cache
// lives, and it used to send no metadata at all.
func TestCodingPlanRequestNamesTheSession(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode body: %v", err)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicStream))
	}))
	defer server.Close()

	store := &memStore{items: map[string][]byte{
		"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"zai","api_key":"abc.def"}`),
	}}
	client := NewClient(store)
	client.http = server.Client()
	client.http.Transport = rewriteTransport{server: server.URL, base: client.http.Transport}

	ctx := providers.WithSessionHint(context.Background(), "conversation-42")
	resp, _, err := client.ChatStream(ctx, "acc1", translate.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer resp.Body.Close()

	metadata, _ := got["metadata"].(map[string]any)
	if metadata == nil {
		t.Fatalf("coding-plan request carried no metadata: %#v", got)
	}
	device := deviceMid("acc1")
	want := planUserID(device, sessionOf("conversation-42", device))
	if metadata["user_id"] != want {
		t.Errorf("metadata.user_id=%v want %s", metadata["user_id"], want)
	}
	// A coding-plan credential keeps its own body shape: no plan identity blocks.
	if _, exists := got["system"].([]any); exists {
		t.Error("api-key requests must not be dressed as the plan channel")
	}
}

// TestPlanRequestNamesTheConversation pins the JWT channel's session: the plan
// gateway already gets metadata.user_id, and it used to carry an empty
// session_id on every request.
func TestPlanRequestNamesTheConversation(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode body: %v", err)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicStream))
	}))
	defer server.Close()

	store := &memStore{items: map[string][]byte{
		"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"oauth","provider":"zai","zcode_jwt_token":"jwt-abc"}`),
	}}
	client := NewClient(store)
	client.http = server.Client()
	client.http.Transport = rewriteTransport{server: server.URL, base: client.http.Transport}

	ctx := providers.WithSessionHint(context.Background(), "conversation-42")
	resp, _, err := client.ChatStream(ctx, "acc1", translate.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer resp.Body.Close()

	metadata, _ := got["metadata"].(map[string]any)
	raw, _ := metadata["user_id"].(string)
	var named struct {
		DeviceID  string `json:"device_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(raw), &named); err != nil {
		t.Fatalf("metadata.user_id=%q is not the client's JSON string: %v", raw, err)
	}
	device := deviceMid("acc1")
	if named.DeviceID != device {
		t.Errorf("device_id=%q want %q", named.DeviceID, device)
	}
	if named.SessionID != sessionOf("conversation-42", device) {
		t.Errorf("session_id=%q want the conversation's session", named.SessionID)
	}
	if named.SessionID == "" {
		t.Error("the plan channel must name its conversation")
	}
}
