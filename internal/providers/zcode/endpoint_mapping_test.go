package zcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

// agentConfigsAnswer is the shape the upstream publishes: the table lives under
// data.proxyEndpoint.mapping as from/to pairs.
const agentConfigsAnswer = `{
  "code": 0,
  "data": {
    "proxyEndpoint": {
      "mapping": [
        {"from": "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages/", "to": "https://zcode.z.ai/api/v1/ultra-zai/v1/messages"},
        {"from": "https://ZCode.z.ai:443/api/v1/zcode-plan/anthropic/v1/chat", "to": "https://zcode.z.ai:443/api/v1/ultra/v1/chat?ignored=1"},
        {"from": "http://insecure.example/v1/messages", "to": "https://zcode.z.ai/v1/messages"},
        {"from": "https://a.example/v1", "to": "http://still-insecure.example/v1"},
        {"from": "", "to": ""}
      ]
    }
  }
}`

// TestParseEndpointMapping normalizes the table the way the client does: the
// key is origin+path with a lowercase host, the default port stated, and no
// trailing slash; a target keeps only its origin and path.
func TestParseEndpointMapping(t *testing.T) {
	table := parseEndpointMapping([]byte(agentConfigsAnswer))
	if len(table) != 2 {
		t.Fatalf("expected only the two https entries, got %d: %v", len(table), table)
	}
	key := "https://zcode.z.ai:443/api/v1/zcode-plan/anthropic/v1/messages"
	if got := table[key]; got != "https://zcode.z.ai/api/v1/ultra-zai/v1/messages" {
		t.Fatalf("trailing slash was not normalized away: %q -> %q", key, got)
	}
	key = "https://zcode.z.ai:443/api/v1/zcode-plan/anthropic/v1/chat"
	if got := table[key]; got != "https://zcode.z.ai/api/v1/ultra/v1/chat" {
		t.Fatalf("target query should be dropped: %q", got)
	}
}

// TestMapEndpointRewritesAndKeepsQuery: the rewrite replaces origin+path and the
// caller's own query survives, because it is part of the request, not the route.
func TestMapEndpointRewritesAndKeepsQuery(t *testing.T) {
	client := NewClient(&memStore{})
	client.mappings = map[string]mappingSnapshot{
		"https://zcode.z.ai": {
			table: parseEndpointMapping([]byte(agentConfigsAnswer)),
			at:    time.Now(),
		},
	}
	got := client.mapEndpoint("https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages?beta=true")
	if want := "https://zcode.z.ai/api/v1/ultra-zai/v1/messages?beta=true"; got != want {
		t.Fatalf("mapped URL = %q, want %q", got, want)
	}
}

// TestMapEndpointIsFailOpen: every way of not knowing the answer has to leave
// the request alone. A mapping feature that can break a request on its own is
// worse than no mapping at all.
func TestMapEndpointIsFailOpen(t *testing.T) {
	url := "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"

	client := NewClient(&memStore{})
	if got := client.mapEndpoint(url); got != url {
		t.Fatalf("no snapshot should not change the URL: %q", got)
	}

	client.mappings = map[string]mappingSnapshot{
		"https://zcode.z.ai": {table: map[string]string{}, at: time.Now()},
	}
	if got := client.mapEndpoint(url); got != url {
		t.Fatalf("an empty table should not change the URL: %q", got)
	}

	client.mappings["https://zcode.z.ai"] = mappingSnapshot{
		table: parseEndpointMapping([]byte(agentConfigsAnswer)),
		at:    time.Now(),
	}
	if got := client.mapEndpoint("https://other.example/api/v1/zcode-plan/anthropic/v1/messages"); got !=
		"https://other.example/api/v1/zcode-plan/anthropic/v1/messages" {
		t.Fatalf("an unmatched URL should not change: %q", got)
	}
}

// TestEndpointMappingKillSwitch: the switch is the escape hatch for a table the
// upstream got wrong, so it has to stop both the fetch and the rewrite.
func TestEndpointMappingKillSwitch(t *testing.T) {
	t.Setenv("ZCODE_DISABLE_ENDPOINT_MAPPING", "1")
	client := NewClient(&memStore{})
	client.mappings = map[string]mappingSnapshot{
		"https://zcode.z.ai": {
			table: parseEndpointMapping([]byte(agentConfigsAnswer)),
			at:    time.Now(),
		},
	}
	url := "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"
	if got := client.mapEndpoint(url); got != url {
		t.Fatalf("the kill switch must stop the rewrite: %q", got)
	}

	var fetched bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched = true
		_, _ = w.Write([]byte(agentConfigsAnswer))
	}))
	defer server.Close()
	if err := client.fetchEndpointMapping(context.Background(), "acct-1", Credential{}, server.URL); err != nil {
		t.Fatalf("fetchEndpointMapping: %v", err)
	}
	if fetched {
		t.Fatal("the kill switch must stop the fetch")
	}
}

// TestFetchEndpointMappingKeepsTheOldTableOnFailure: not being able to read the
// table is not the same as the upstream having published an empty one.
func TestFetchEndpointMappingKeepsTheOldTableOnFailure(t *testing.T) {
	failing := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentConfigsPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("app_version") == "" || r.URL.Query().Get("platform") == "" {
			t.Errorf("the configs call must name the client build: %s", r.URL.RawQuery)
		}
		if failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(agentConfigsAnswer))
	}))
	defer server.Close()

	client := NewClient(&memStore{})
	client.mappings = map[string]mappingSnapshot{}
	if err := client.fetchEndpointMapping(context.Background(), "acct-1", Credential{APIKey: "k"}, server.URL); err != nil {
		t.Fatalf("fetchEndpointMapping: %v", err)
	}
	if got := len(client.mappings[server.URL].table); got != 2 {
		t.Fatalf("table was not stored: %d entries", got)
	}

	failing = true
	if err := client.fetchEndpointMapping(context.Background(), "acct-1", Credential{APIKey: "k"}, server.URL); err != nil {
		t.Fatalf("a failed fetch must not be an error: %v", err)
	}
	if got := len(client.mappings[server.URL].table); got != 2 {
		t.Fatalf("a failed fetch must keep the previous table, now %d entries", got)
	}
}

// TestEndpointMappingTableIsBounded keeps upstream-controlled data from being
// an unbounded allocation.
func TestEndpointMappingTableIsBounded(t *testing.T) {
	entries := make([]string, 0, endpointMappingMaxEntries+50)
	for i := 0; i < endpointMappingMaxEntries+50; i++ {
		entries = append(entries, `{"from":"https://zcode.z.ai/v1/`+strings.Repeat("a", 1)+string(rune('a'+i%26))+string(rune('0'+i/26%10))+`","to":"https://zcode.z.ai/v2/`+string(rune('a'+i%26))+`"}`)
	}
	payload := `{"data":{"proxyEndpoint":{"mapping":[` + strings.Join(entries, ",") + `]}}}`
	if got := len(parseEndpointMapping([]byte(payload))); got > endpointMappingMaxEntries {
		t.Fatalf("table is not bounded: %d entries", got)
	}
}

// TestRefreshEndpointMappingSkipsFreshSnapshots: the hot path must not fetch on
// every request, only when the snapshot is missing or stale.
func TestRefreshEndpointMappingSkipsFreshSnapshots(t *testing.T) {
	client := NewClient(&memStore{})
	origin := "https://zcode.z.ai"
	client.mappings = map[string]mappingSnapshot{origin: {table: map[string]string{}, at: time.Now()}}
	if !endpointMappingStale(client.mappings[origin]) {
		t.Log("fresh snapshot is not stale")
	}
	stale := mappingSnapshot{at: time.Now().Add(-2 * endpointMappingTTL)}
	if !endpointMappingStale(stale) {
		t.Fatal("an old snapshot must be refreshed")
	}
	empty := mappingSnapshot{}
	if !endpointMappingStale(empty) {
		t.Fatal("a missing snapshot must be fetched")
	}
}

// TestChatRequestUsesTheMappedEndpoint closes the loop: the rewrite has to be
// visible on the request the chat path actually builds, not just in a unit call.
func TestChatRequestUsesTheMappedEndpoint(t *testing.T) {
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
	}))
	defer catalog.Close()

	store := &memStore{items: map[string][]byte{
		"acc1": []byte(`{"format":"zcode-credential-v1","auth_mode":"oauth","provider":"zai","zcode_jwt_token":"jwt-abc"}`),
	}}
	client := NewClient(store)
	client.catalogURL = catalog.URL
	client.mappings = map[string]mappingSnapshot{
		"https://zcode.z.ai": {
			table: map[string]string{
				"https://zcode.z.ai:443/api/v1/zcode-plan/anthropic/v1/messages": "https://zcode.z.ai/api/v1/ultra-zai/v1/messages",
			},
			at: time.Now(),
		},
	}
	cred := Credential{Format: CredentialFormat, AuthMode: AuthModeOAuth, Provider: "zai", ZCodeJWT: "jwt-abc"}
	req, _, err := client.chatRequest(context.Background(), "acc1", cred, translate.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("chatRequest: %v", err)
	}
	if got := req.URL.Path; got != "/api/v1/ultra-zai/v1/messages" {
		t.Fatalf("chat request did not follow the upstream mapping: %s", got)
	}
}
