package antigravity

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

// catalogEntry describes one model the Antigravity IDE exposes. Antigravity
// serves frontier Gemini models plus selected Claude and GPT-OSS checkpoints;
// the list below is a curated snapshot, see the Antigravity models docs.
type catalogEntry struct {
	// ID is the model id accepted by the OpenAI-compat layer.
	ID string `json:"id"`
	// Family picks the capability rules: "gemini" accepts reasoning_effort,
	// "claude" and "gpt" keep upstream thinking and never get an effort value.
	Family string `json:"family"`
	// ContextWindow is the input token budget.
	ContextWindow int `json:"context_window"`
	// MaxOutput caps completion tokens.
	MaxOutput int `json:"max_output_tokens"`
	// Images reports image input support.
	Images bool `json:"images"`
	// Reasoning reports a thinking mode.
	Reasoning bool `json:"reasoning"`
	// DisplayName is the console-facing label.
	DisplayName string `json:"display_name,omitempty"`
}

var catalog = []catalogEntry{
	{ID: "gemini-3.8-flash", Family: "gemini", ContextWindow: 1_048_576, MaxOutput: 65536, Images: true, Reasoning: true},
	{ID: "gemini-3.7-flash", Family: "gemini", ContextWindow: 1_048_576, MaxOutput: 65536, Images: true, Reasoning: true},
	{ID: "gemini-3.6-flash", Family: "gemini", ContextWindow: 1_048_576, MaxOutput: 65536, Images: true, Reasoning: true},
	{ID: "gemini-3.1-pro", Family: "gemini", ContextWindow: 1_048_576, MaxOutput: 65536, Images: true, Reasoning: true},
	{ID: "claude-sonnet-5.5", Family: "claude", ContextWindow: 500_000, MaxOutput: 32000, Images: true, Reasoning: true},
	{ID: "claude-opus-5.5", Family: "claude", ContextWindow: 500_000, MaxOutput: 32000, Images: true, Reasoning: true},
	{ID: "claude-sonnet-4.6", Family: "claude", ContextWindow: 500_000, MaxOutput: 32000, Images: true, Reasoning: true},
	{ID: "claude-opus-4.6", Family: "claude", ContextWindow: 500_000, MaxOutput: 32000, Images: true, Reasoning: true},
	{ID: "gpt-oss-120b", Family: "gpt", ContextWindow: 131_072, MaxOutput: 65536, Images: true, Reasoning: true},
}

// ParseCatalogJSON maps the upstream OpenAI-compatible model list onto
// ModelInfo when Gemini's OpenAI-compat layer publishes it; otherwise the
// curated static catalog stands in. An explicit upstream list wins when it is
// non-empty and yields at least one known or gemini-prefixed model.
func ParseCatalogJSON(raw []byte) ([]providers.ModelInfo, error) {
	var env struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("antigravity catalog: %w", err)
	}
	if len(env.Data) == 0 {
		return nil, nil
	}
	models := make([]providers.ModelInfo, 0, len(env.Data))
	seen := map[string]struct{}{}
	for _, entry := range env.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		info, ok := entryModelInfo(id)
		if !ok {
			continue
		}
		models = append(models, info)
	}
	if len(models) == 0 {
		return nil, nil
	}
	return models, nil
}

// catalogModelInfos returns the curated model list for the console catalog.
func catalogModelInfos() []providers.ModelInfo {
	models := make([]providers.ModelInfo, 0, len(catalog))
	for _, entry := range catalog {
		info, ok := entryInfo(entry)
		if ok {
			models = append(models, info)
		}
	}
	return models
}

// entryModelInfo resolves an upstream model id to its capabilities. Unknown
// gemini-* ids get default Gemini caps; unknown claude/gpt ids are dropped.
func entryModelInfo(id string) (providers.ModelInfo, bool) {
	for _, entry := range catalog {
		if entry.ID == id {
			return entryInfo(entry)
		}
	}
	lower := strings.ToLower(strings.TrimSpace(id))
	if strings.HasPrefix(lower, "gemini-") {
		return entryInfo(catalogEntry{ID: id, Family: "gemini", ContextWindow: 1_048_576, MaxOutput: DefaultMaxTokens, Images: true, Reasoning: true})
	}
	return providers.ModelInfo{}, false
}

func entryInfo(entry catalogEntry) (providers.ModelInfo, bool) {
	if strings.TrimSpace(entry.ID) == "" {
		return providers.ModelInfo{}, false
	}
	caps := providers.ModelCapabilities{
		ContextWindow: entry.ContextWindow,
		MaxOutput:     entry.MaxOutput,
		Tools:         true,
		Images:        entry.Images,
		Reasoning:     entry.Reasoning,
	}
	if entry.Family == "gemini" {
		caps.ReasoningOptions = []string{"low", "medium", "high"}
		caps.ReasoningDefault = "medium"
		caps.ReasoningType = "effort"
		caps.CanDisableThinking = false
	}
	return providers.ModelInfo{
		NativeModel:  entry.ID,
		PublicModel:  entry.ID,
		DisplayName:  firstNonEmpty(entry.DisplayName, entry.ID),
		Capabilities: caps,
	}, true
}

func (c *Client) fetchCatalog(ctx context.Context, accountID string, credential Credential) ([]providers.ModelInfo, error) {
	base, err := credential.ChatBase()
	if err != nil {
		return nil, err
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(base, PathModels), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, newProviderError(resp.StatusCode, string(body))
	}
	models, err := ParseCatalogJSON(body)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		models = catalogModelInfos()
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("antigravity catalog produced no models")
	}
	return models, nil
}

// Models implements providers.ModelCatalogProvider. The list is per account
// (keyed by credential) because the OpenAI-compat layer may publish different
// models for different keys.
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	if models, ok := c.cachedModels(accountID, timeNow()); ok {
		return models, nil
	}
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	models, err := c.fetchCatalog(ctx, accountID, credential)
	if err != nil {
		return nil, err
	}
	c.rememberModels(accountID, models, timeNow())
	return cloneModels(models), nil
}

func (c *Client) cachedModels(accountID string, now time.Time) ([]providers.ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.catalogs[accountID]
	if !ok || len(entry.models) == 0 || now.Sub(entry.at) > catalogTTL {
		return nil, false
	}
	return cloneModels(entry.models), true
}

func (c *Client) rememberModels(accountID string, models []providers.ModelInfo, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalogs == nil {
		c.catalogs = map[string]catalogCache{}
	}
	c.catalogs[accountID] = catalogCache{models: cloneModels(models), at: at}
}

func (c *Client) lookupModel(accountID, model string) (providers.ModelInfo, bool) {
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" {
		return providers.ModelInfo{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.catalogs[accountID]
	if !ok {
		return providers.ModelInfo{}, false
	}
	for _, info := range entry.models {
		if strings.ToLower(info.NativeModel) == want || strings.ToLower(info.PublicModel) == want {
			return info, true
		}
	}
	return providers.ModelInfo{}, false
}

func cloneModels(in []providers.ModelInfo) []providers.ModelInfo {
	if len(in) == 0 {
		return nil
	}
	out := make([]providers.ModelInfo, len(in))
	for i, model := range in {
		out[i] = model
		if len(model.Capabilities.ReasoningOptions) > 0 {
			out[i].Capabilities.ReasoningOptions = append([]string(nil), model.Capabilities.ReasoningOptions...)
		}
	}
	return out
}
