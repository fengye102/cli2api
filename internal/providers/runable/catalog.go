package runable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// catalogEnvelope is the RPC wrapper the model list arrives in: the payload sits
// under "json", not at the top level.
type catalogEnvelope struct {
	JSON struct {
		Models []catalogEntry `json:"models"`
	} `json:"json"`
	Models []catalogEntry `json:"models"`
}

type catalogEntry struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Object            string   `json:"object"`
	ModelProvider     string   `json:"modelProvider"`
	Type              string   `json:"type"`
	ContextWindow     int      `json:"context_window"`
	MaxTokens         int      `json:"max_tokens"`
	SupportsChatMode  *bool    `json:"supportsChatMode"`
	SupportsAgentMode bool     `json:"supportsAgentMode"`
	IsFree            bool     `json:"isFree"`
	Tags              []string `json:"tags"`
}

// fetchModels posts the (empty) catalog query. It returns the status and body;
// interpretation is the caller's job.
func (c *Client) fetchModels(ctx context.Context, accountID string, cred Credential) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin()+ModelsPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, nil, err
	}
	applyHeaders(req.Header, cred)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("runable 目录不可达：%w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("runable 目录读取失败：%w", err)
	}
	return resp.StatusCode, body, nil
}

// Models returns the account's catalog, refreshing it when the TTL has expired.
// Unlike MiniMax there is no static fallback: the vendor's list is the only
// source of truth for which models exist, and inventing one would route requests
// the account cannot serve.
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	if models, ok := c.cachedCatalog(accountID); ok {
		return models, nil
	}
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	status, body, err := c.fetchModels(ctx, accountID, cred)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, newProviderError(status, string(body))
	}
	models, err := parseCatalog(body)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("runable 目录里没有可聊天的模型")
	}
	c.storeCatalog(accountID, models)
	return models, nil
}

// parseCatalog keeps the models the chat endpoint can actually serve: language
// models with chat mode enabled. Image/video models are skipped — a chat request
// naming one would only earn a refusal.
func parseCatalog(body []byte) ([]providers.ModelInfo, error) {
	var envelope catalogEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("runable catalog: %w", err)
	}
	entries := envelope.JSON.Models
	if len(entries) == 0 {
		entries = envelope.Models
	}
	models := make([]providers.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		if entry.Type != "" && entry.Type != "language" {
			continue
		}
		if entry.SupportsChatMode != nil && !*entry.SupportsChatMode {
			continue
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" || strings.EqualFold(name, id) {
			name = id
		}
		models = append(models, providers.ModelInfo{
			NativeModel: id,
			PublicModel: id,
			DisplayName: name,
			Free:        entry.IsFree,
			Capabilities: providers.ModelCapabilities{
				ContextWindow: entry.ContextWindow,
				MaxOutput:     entry.MaxTokens,
				// The chat endpoint takes one text prompt: no tools, no image
				// blocks, and the adapter sends no thinking config.
				Tools:     false,
				Images:    false,
				Reasoning: false,
			},
		})
	}
	return models, nil
}

func (c *Client) cachedCatalog(accountID string) ([]providers.ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.catalogs[accountID]
	if !ok || time.Since(cached.at) > CatalogTTL {
		return nil, false
	}
	return cached.models, true
}

func (c *Client) storeCatalog(accountID string, models []providers.ModelInfo) {
	c.mu.Lock()
	c.catalogs[accountID] = catalogCache{models: models, at: time.Now()}
	c.mu.Unlock()
}
