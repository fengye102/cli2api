package zcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

// TestDumpPlanRequestShape prints the request the client actually sends, so the
// three-block contract can be inspected without an upstream account.
func TestDumpPlanRequestShape(t *testing.T) {
	var captured map[string]any
	var hdr http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		hdr = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&captured)
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

	resp, _, err := client.ChatStream(context.Background(), "acc1", translate.ChatRequest{
		Model: "GLM-5.3-Flash",
		Messages: []translate.ChatMessage{
			{Role: "system", Content: "you are a terse assistant"},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resp.Body.Close()

	blocks, _ := captured["system"].([]any)
	digest := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:8])
	}
	fmt.Printf("\n--- 实际发出的请求 ---\n")
	fmt.Printf("path            : %s\n", server.URL)
	fmt.Printf("system 块数      : %d\n", len(blocks))
	for i, b := range blocks {
		blk, _ := b.(map[string]any)
		text, _ := blk["text"].(string)
		_, cached := blk["cache_control"]
		fmt.Printf("  [%d] len=%-5d sha8=%s cached=%v\n", i, len(text), digest(text), cached)
		if i == 2 {
			fmt.Printf("      head: %q\n", firstN(text, 90))
			fmt.Printf("      tail: %q\n", lastN(text, 60))
		}
	}
	meta, _ := captured["metadata"].(map[string]any)
	fmt.Printf("metadata.user_id: %v\n", meta["user_id"])
	fmt.Printf("x-zcode-query-source: %q\n", hdr.Get("x-zcode-query-source"))
	fmt.Printf("x-zcode-session-type: %q\n", hdr.Get("x-zcode-session-type"))
	fmt.Printf("X-Device-Mid    : %q\n", hdr.Get("X-Device-Mid"))
	fmt.Printf("----------------------\n\n")

	if len(blocks) != 3 {
		t.Fatalf("system block count=%d want 3", len(blocks))
	}
	third, _ := blocks[2].(map[string]any)
	text, _ := third["text"].(string)
	if !strings.Contains(text, "you are a terse assistant") {
		t.Error("caller prompt missing from third block")
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
