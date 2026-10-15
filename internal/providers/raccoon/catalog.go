package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// catalogEntry is one model record from /model_catalog. Field names follow the
// upstream payload, which is snake_case in the live response but camelCase in
// the client's own presets, so both spellings are tolerated.
type catalogEntry struct {
	Name             string   `json:"name"`
	ModelName        string   `json:"model_name"`
	DisplayName      string   `json:"displayName"`
	DisplayNameSnake string   `json:"display_name"`
	Description      string   `json:"description"`
	Tags             []string `json:"tags"`
	ContextWindow    *int     `json:"contextWindow"`
	ContextSnake     *int     `json:"context_window"`
	MaxTokens        *int     `json:"maxTokens"`
	MaxTokensSnake   *int     `json:"max_tokens"`
	SupportImage     *bool    `json:"supportImage"`
	SupportThinking  *bool    `json:"supportThinking"`
	// BillingMultiplier / BillingEffectiveMultiplier are the upstream price
	// multipliers (list price and the effective price after any promotion).
	// The effective value wins, falling back to the list price.
	BillingMultiplier *float64 `json:"billing_multiplier"`
	BillingEffective  *float64 `json:"billing_effective_multiplier"`
	Params            *struct {
		ContextWindow  *int `json:"context_window"`
		ContextWindowC *int `json:"contextWindow"`
		MaxTokens      *int `json:"max_tokens"`
		MaxTokensC     *int `json:"maxTokens"`
	} `json:"params"`
}

// creditsOf renders the upstream billing multiplier in the shared
// "x0.5 credits" shape. The effective (post-promotion) value wins; the list
// price stands in when the response predates that field. No value → empty
// string, which the console renders as a dash rather than a fabricated price.
func (e catalogEntry) creditsOf() string {
	rate := 0.0
	switch {
	case e.BillingEffective != nil:
		rate = *e.BillingEffective
	case e.BillingMultiplier != nil:
		rate = *e.BillingMultiplier
	}
	return creditsText(rate)
}

// isFree reports whether upstream is currently serving this model at no charge.
// A promotion that zeroes the effective multiplier ("限免") is the upstream's
// own statement of that, so the console can show its free badge without the
// gateway guessing. Only an explicit zero counts: an absent field means
// "unknown", not "free".
func (e catalogEntry) isFree() bool {
	return e.BillingEffective != nil && *e.BillingEffective == 0
}

// entryName resolves the model id: `name` first (that is what upstream routes
// on), then `model_name` as the older spelling.
func (e catalogEntry) entryName() string {
	if name := strings.TrimSpace(e.Name); name != "" {
		return name
	}
	return strings.TrimSpace(e.ModelName)
}

// contextWindowOf reports the context ceiling. It may live at the top level or
// nested under `params`; both spellings of each are checked.
func (e catalogEntry) contextWindowOf() int {
	for _, v := range []*int{e.ContextWindow, e.ContextSnake} {
		if v != nil && *v > 0 {
			return *v
		}
	}
	if e.Params != nil {
		for _, v := range []*int{e.Params.ContextWindow, e.Params.ContextWindowC} {
			if v != nil && *v > 0 {
				return *v
			}
		}
	}
	return 0
}

// maxTokensOf reports the output ceiling, with the same two-level lookup.
func (e catalogEntry) maxTokensOf() int {
	for _, v := range []*int{e.MaxTokens, e.MaxTokensSnake} {
		if v != nil && *v > 0 {
			return *v
		}
	}
	if e.Params != nil {
		for _, v := range []*int{e.Params.MaxTokens, e.Params.MaxTokensC} {
			if v != nil && *v > 0 {
				return *v
			}
		}
	}
	return 0
}

// supportsImages reports vision support. The catalog has no explicit field in
// the live shape, so an explicit boolean wins when present and tags are the
// fallback signal.
func (e catalogEntry) supportsImages() bool {
	if e.SupportImage != nil {
		return *e.SupportImage
	}
	for _, tag := range e.Tags {
		switch strings.TrimSpace(strings.ToLower(tag)) {
		case "vision", "image", "image-understanding":
			return true
		}
	}
	return false
}

// supportsThinking reports reasoning support. An explicit boolean wins; the
// live catalog omits the field, and the client treats every catalog model as
// thinking-capable, so the default is true.
func (e catalogEntry) supportsThinking() bool {
	if e.SupportThinking != nil {
		return *e.SupportThinking
	}
	return true
}

// displayNameOf prefers `displayName` / `display_name`, then the short
// `description` codename (e.g. "Raccoon-Work"), then the model id.
func (e catalogEntry) displayNameOf() string {
	if name := firstNonEmpty(e.DisplayName, e.DisplayNameSnake); name != "" {
		return name
	}
	if code := strings.TrimSpace(e.Description); code != "" && len([]rune(code)) <= 64 {
		return code
	}
	return e.entryName()
}

// parseCatalogEntries flattens the /model_catalog payload into model records.
//
// The live shape nests the models two levels deep:
//
//	{ "code": ..., "data": { "categories": [ { "type": ..., "models": [ ... ] } ] } }
//
// Older captures (and the client's own presets) instead put the list directly
// on `data`, or return a bare array. All three are accepted, because a
// mismatched assumption here fails the whole account probe, not just the model
// list. Duplicate ids keep the first occurrence, matching the client.
func parseCatalogEntries(raw []byte) ([]catalogEntry, error) {
	var root json.RawMessage = raw
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}

	// Bare array: [ {...}, {...} ]
	if strings.HasPrefix(trimmed, "[") {
		var list []catalogEntry
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		return dedupeEntries(list), nil
	}

	var envelope struct {
		Code json.RawMessage `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}

	// `data` may be an array (older shape) or an object (live shape). When the
	// key is absent entirely, treat the root object itself as the container.
	data := envelope.Data
	if len(data) == 0 || string(data) == "null" {
		data = root
	}

	// Array under `data`: [ {...} ]
	if strings.HasPrefix(strings.TrimSpace(string(data)), "[") {
		var list []catalogEntry
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
		return dedupeEntries(list), nil
	}

	// Object under `data`: either { categories: [...] } or a single model, or
	// a direct { models: [...] } container.
	var container struct {
		Models     []catalogEntry `json:"models"`
		Categories []struct {
			Models []catalogEntry `json:"models"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(data, &container); err != nil {
		return nil, err
	}

	var out []catalogEntry
	out = append(out, container.Models...)
	for _, category := range container.Categories {
		out = append(out, category.Models...)
	}
	if len(out) == 0 {
		// The container object may itself be a single model record.
		var single catalogEntry
		if err := json.Unmarshal(data, &single); err != nil {
			return nil, err
		}
		if single.entryName() != "" {
			out = append(out, single)
		}
	}
	return dedupeEntries(out), nil
}

// dedupeEntries drops records without an id and keeps the first occurrence of
// each id, preserving upstream order.
func dedupeEntries(entries []catalogEntry) []catalogEntry {
	seen := make(map[string]struct{}, len(entries))
	out := make([]catalogEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.entryName()
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, entry)
	}
	return out
}

// fetchCatalog pulls the remote model catalog and normalizes it into
// ModelInfo entries. On failure it returns a *providers.Error so Probe can
// surface it.
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
	entries, err := parseCatalogEntries(raw)
	if err != nil {
		return nil, fmt.Errorf("raccoon catalog: malformed response: %w", err)
	}
	models := make([]providers.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		models = append(models, providers.ModelInfo{
			NativeModel: entry.entryName(), PublicModel: entry.entryName(), DisplayName: entry.displayNameOf(),
			Credits: entry.creditsOf(),
			Free:    entry.isFree(),
			Capabilities: providers.ModelCapabilities{
				ContextWindow: entry.contextWindowOf(), MaxOutput: entry.maxTokensOf(),
				Tools: true, Images: entry.supportsImages(), Reasoning: entry.supportsThinking(),
			},
		})
	}
	if len(models) == 0 {
		return fallbackModels(), nil
	}
	return models, nil
}

// creditsText renders the upstream billing multiplier in the shared
// "x0.5 credits" shape the console already formats.
func creditsText(rate float64) string {
	if rate <= 0 {
		return ""
	}
	return "x" + strconv.FormatFloat(rate, 'f', -1, 64) + " credits"
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
