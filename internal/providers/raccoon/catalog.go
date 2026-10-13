package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// fallbackModels is the static catalog used when the remote catalog cannot be
// fetched (mirrors the Raccoon client's preset list + model_catalog content).
func fallbackModels() []providers.ModelInfo {
	return []providers.ModelInfo{
		{
			NativeModel: "raccoon-8c4485", PublicModel: "raccoon-8c4485", DisplayName: "Raccoon-Work",
			Capabilities: providers.ModelCapabilities{ContextWindow: 1000000, MaxOutput: 100000, Tools: true, Images: true, Reasoning: false},
		},
		{
			NativeModel: "raccoon-19b265", PublicModel: "raccoon-19b265", DisplayName: "Raccoon-Work-260817-A",
			Capabilities: providers.ModelCapabilities{ContextWindow: 1000000, MaxOutput: 100000, Tools: true, Images: false, Reasoning: false},
		},
		{
			NativeModel: "raccoon-405a1c", PublicModel: "raccoon-405a1c", DisplayName: "Raccoon-Work-260817-B",
			Capabilities: providers.ModelCapabilities{ContextWindow: 1000000, MaxOutput: 100000, Tools: true, Images: false, Reasoning: false},
		},
		{
			NativeModel: "raccoon-chat-ml-5-5", PublicModel: "raccoon-chat-ml-5-5", DisplayName: "Raccoon Chat",
			Capabilities: providers.ModelCapabilities{ContextWindow: 180000, MaxOutput: 80000, Tools: true, Images: false, Reasoning: true},
		},
		{
			NativeModel: "sn-sensenova-6-8-flash-lite", PublicModel: "sn-sensenova-6-8-flash-lite", DisplayName: "SenseNova 6.8 Flash Lite",
			Capabilities: providers.ModelCapabilities{ContextWindow: 256000, MaxOutput: 63999, Tools: true, Images: false, Reasoning: false},
		},
	}
}

// fetchCatalog pulls the remote model catalog and normalizes it into
// ModelInfo entries. On failure it returns a *providers.Error so Probe can
// surface it. The catalog shape uses `name` as the model id, `displayName` as
// the display name, contextWindow → context, maxTokens → max output.
func (c *Client) fetchCatalog(ctx context.Context, accountID string, cred Credential) ([]providers.ModelInfo, error) {
	endpoint := strings.TrimRight(c.llmBase, "/") + catalogPath
	ctx, cancel := context.WithTimeout(ctx, time.Duration(probeTimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	token := strings.TrimSpace(cred.AccessToken)
	if token == "" {
		return nil, fmt.Errorf("raccoon catalog needs an access token")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		cls := Classify(resp.StatusCode, string(raw))
		return nil, &providers.Error{Kind: cls.Kind, Status: resp.StatusCode, Message: cls.Message}
	}
	var payload struct {
		Data []struct {
			Name            string `json:"name"`
			DisplayName     string `json:"displayName"`
			ContextWindow   int    `json:"contextWindow"`
			MaxTokens       int    `json:"maxTokens"`
			SupportImage    bool   `json:"supportImage"`
			SupportThinking bool   `json:"supportThinking"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		// Some responses are a bare array; tolerate that.
		var list []struct {
			Name            string `json:"name"`
			DisplayName     string `json:"displayName"`
			ContextWindow   int    `json:"contextWindow"`
			MaxTokens       int    `json:"maxTokens"`
			SupportImage    bool   `json:"supportImage"`
			SupportThinking bool   `json:"supportThinking"`
		}
		if err2 := json.Unmarshal(raw, &list); err2 != nil {
			return nil, fmt.Errorf("raccoon catalog: malformed response: %w", err)
		}
		payload.Data = list
	}
	models := make([]providers.ModelInfo, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.Name == "" {
			continue
		}
		models = append(models, providers.ModelInfo{
			NativeModel: m.Name, PublicModel: m.Name, DisplayName: firstNonEmpty(m.DisplayName, m.Name),
			Capabilities: providers.ModelCapabilities{
				ContextWindow: m.ContextWindow, MaxOutput: m.MaxTokens,
				Tools: true, Images: m.SupportImage, Reasoning: m.SupportThinking,
			},
		})
	}
	if len(models) == 0 {
		return fallbackModels(), nil
	}
	return models, nil
}

// Models returns the catalog for an account: the cached remote list when fresh,
// otherwise a fresh fetch, otherwise the static fallback.
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	c.mu.Lock()
	cached, ok := c.catalogs[accountID]
	c.mu.Unlock()
	if ok && time.Since(cached.at) < catalogCacheTTL*time.Second {
		return cached.models, nil
	}
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return fallbackModels(), nil
	}
	models, err := c.fetchCatalog(ctx, accountID, cred)
	if err != nil {
		// Keep serving the stale cache; fall back to static otherwise.
		if ok {
			return cached.models, nil
		}
		return fallbackModels(), nil
	}
	c.mu.Lock()
	c.catalogs[accountID] = catalogCache{models: models, at: time.Now()}
	c.mu.Unlock()
	return models, nil
}
