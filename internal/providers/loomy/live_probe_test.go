//go:build probe

// Live probes against the real Loomy gateway, run by hand inside the deployed
// container. Excluded from normal builds: it reads the production database.
//
//	go test -tags probe -c -o /tmp/loomy_probe.test ./internal/providers/loomy
//	/tmp/loomy_probe.test -test.run TestProbeImageModels -test.v
package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/store"
)

func probeClient(t *testing.T) (*Client, string) {
	t.Helper()
	path := os.Getenv("LOOMY_PROBE_DB")
	if path == "" {
		path = "/data/qoder.db"
	}
	st, err := store.OpenStore(path)
	if err != nil {
		t.Fatalf("open store %s: %v", path, err)
	}
	client := NewClient(st)
	accountID := os.Getenv("LOOMY_PROBE_ACCOUNT")
	if accountID == "" {
		accounts, err := st.List(context.Background())
		if err != nil {
			t.Fatalf("list accounts: %v", err)
		}
		for _, a := range accounts {
			if a.Provider == "loomy" {
				accountID = a.ID
				break
			}
		}
	}
	if accountID == "" {
		t.Fatal("no loomy account")
	}
	return client, accountID
}

func probeCall(t *testing.T, client *Client, accountID, path string, body any) {
	t.Helper()
	ctx := context.Background()
	cred, err := client.credential(ctx, accountID)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	hc, err := client.modelClient(ctx, accountID)
	if err != nil {
		t.Fatalf("model client: %v", err)
	}
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	endpoint := strings.TrimRight(client.modelBase, "/") + path
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for k, v := range modelHeaders(cred.Session) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	head := string(raw)
	if len(head) > 1500 {
		head = head[:1500] + " …"
	}
	fmt.Printf("\n### POST %s  -> %d %s\n%s\n", path, resp.StatusCode, resp.Header.Get("Content-Type"), head)
}

func TestProbeImageModels(t *testing.T) {
	client, accountID := probeClient(t)
	fmt.Printf("account=%s base=%s\n", accountID, client.modelBase)

	// 1. raw /models: what shape do the generation records really have?
	ctx := context.Background()
	cred, err := client.credential(ctx, accountID)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	hc, err := client.modelClient(ctx, accountID)
	if err != nil {
		t.Fatalf("model client: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, strings.TrimRight(client.modelBase, "/")+modelsPath, nil)
	for k, v := range modelHeaders(cred.Session) {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(raw, &payload)
	fmt.Printf("### GET /models -> %d, %d records\n", resp.StatusCode, len(payload.Data))
	for _, record := range payload.Data {
		id := strings.ToLower(fmt.Sprint(record["id"]))
		if strings.Contains(id, "image") || strings.Contains(id, "seedream") {
			out, _ := json.Marshal(record)
			fmt.Printf("  RAW %s\n", out)
		}
	}

	// 2. the OpenAI-shaped image endpoint the reference implementation proxies.
	probeCall(t, client, accountID, "/images/generations", map[string]any{
		"model":  "Hy-Image-3.5-preview",
		"prompt": "a red apple on a white table, photo",
		"n":      1,
		"size":   "1024x1024",
	})

	// 3. does the chat endpoint serve these models instead (image output)?
	probeCall(t, client, accountID, chatPath, map[string]any{
		"model":    "Hy-Image-3.5-preview",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "画一只橘猫"}},
	})
}
