package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// catalogEnvelope is the live model catalog the agent gateway serves. The model
// list is a MAP keyed by model id, not an array, and the metadata is richer than
// the OpenAI shape: it states whether tools work, how large the window is, and
// which thinking variants the model offers.
type catalogEnvelope struct {
	Version    string `json:"version"`
	TTLSeconds int    `json:"ttlSeconds"`
	Providers  []struct {
		ProviderID string `json:"providerId"`
		Config     struct {
			Name   string                     `json:"name"`
			NPM    string                     `json:"npm"`
			Models map[string]catalogModelDoc `json:"models"`
		} `json:"config"`
	} `json:"providers"`
}

type catalogModelDoc struct {
	Name        string `json:"name"`
	Attachment  bool   `json:"attachment"`
	Reasoning   bool   `json:"reasoning"`
	Temperature bool   `json:"temperature"`
	ToolCall    bool   `json:"tool_call"`
	Limit       struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
}

// getModels performs the catalog GET and returns the status and body. It never
// interprets the result: callers decide whether a 401 means "refresh" or "dead".
func (c *Client) getModels(ctx context.Context, accountID string, cred Credential) (int, []byte, error) {
	site := c.site(cred.Region)
	endpoint := site.AgentHost + fmt.Sprintf(ModelsPath, site.Region)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	applyAuthHeaders(req.Header, cred)
	req.Header.Set("Accept", "application/json")
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("minimax 目录不可达：%w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("minimax 目录读取失败：%w", err)
	}
	return resp.StatusCode, body, nil
}

// Models returns the account's catalog, refreshing it when the TTL has expired.
// A live catalog that fails does not make the account unusable: the static list
// is served instead, and the next refresh tries the gateway again.
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	if models, ok := c.cachedCatalog(accountID); ok {
		return models, nil
	}
	cred, err := c.freshCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	status, body, err := c.getModels(ctx, accountID, cred)
	if err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		if renewed, refreshErr := c.refresh(ctx, accountID, cred); refreshErr == nil {
			cred = renewed
			status, body, err = c.getModels(ctx, accountID, cred)
		}
	}
	if err != nil {
		c.dropCatalog(accountID)
		return staticModels(), nil
	}
	if status >= 300 {
		c.dropCatalog(accountID)
		return staticModels(), nil
	}
	models, parseErr := parseCatalog(body)
	if parseErr != nil || len(models) == 0 {
		return staticModels(), nil
	}
	c.storeCatalog(accountID, models)
	return models, nil
}

// parseCatalog folds the live document into ModelInfo records, keeping a static
// entry's display name when the live one is just the bare id.
func parseCatalog(body []byte) ([]providers.ModelInfo, error) {
	var envelope catalogEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("minimax catalog: %w", err)
	}
	known := map[string]staticModel{}
	for _, entry := range staticCatalog {
		known[strings.ToLower(entry.ID)] = entry
	}
	models := make([]providers.ModelInfo, 0, 8)
	for _, provider := range envelope.Providers {
		ids := make([]string, 0, len(provider.Config.Models))
		for id := range provider.Config.Models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			doc := provider.Config.Models[id]
			entry := known[strings.ToLower(id)]
			name := firstNonEmpty(doc.Name, entry.Name)
			if name == "" || strings.EqualFold(name, id) {
				name = id
			}
			input := doc.Modalities.Input
			models = append(models, providers.ModelInfo{
				NativeModel: id,
				PublicModel: id,
				DisplayName: name,
				Capabilities: providers.ModelCapabilities{
					ContextWindow: doc.Limit.Context,
					MaxOutput:     doc.Limit.Output,
					Tools:         doc.ToolCall || entry.Tools,
					Images:        containsFold(input, "image") || entry.Images,
					Reasoning:     doc.Reasoning || entry.Reasoning,
				},
			})
		}
	}
	return models, nil
}

// staticModels is the fallback catalog, carrying the metadata the live document
// reports for the same ids.
func staticModels() []providers.ModelInfo {
	models := make([]providers.ModelInfo, 0, len(staticCatalog))
	for _, entry := range staticCatalog {
		models = append(models, providers.ModelInfo{
			NativeModel: entry.ID,
			PublicModel: entry.ID,
			DisplayName: entry.Name,
			Capabilities: providers.ModelCapabilities{
				ContextWindow: entry.Context,
				MaxOutput:     entry.MaxOutput,
				Tools:         entry.Tools,
				Images:        entry.Images,
				Reasoning:     entry.Reasoning,
			},
		})
	}
	return models
}

// modelCapabilities looks one model up in the catalog, falling back to the static
// entry so a catalog outage cannot strip a model's limits from the request.
func (c *Client) modelCapabilities(ctx context.Context, accountID, model string) providers.ModelCapabilities {
	if models, err := c.Models(ctx, accountID); err == nil {
		for _, entry := range models {
			if strings.EqualFold(entry.PublicModel, model) || strings.EqualFold(entry.NativeModel, model) {
				return entry.Capabilities
			}
		}
	}
	for _, entry := range staticCatalog {
		if strings.EqualFold(entry.ID, model) {
			return providers.ModelCapabilities{
				ContextWindow: entry.Context,
				MaxOutput:     entry.MaxOutput,
				Tools:         entry.Tools,
				Images:        entry.Images,
				Reasoning:     entry.Reasoning,
			}
		}
	}
	return providers.ModelCapabilities{}
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

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}
