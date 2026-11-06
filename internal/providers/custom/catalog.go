package custom

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

// catalogEntry is one record from GET {base}/models. Relays copy the OpenAI
// shape; the size fields are optional and spelled differently by different
// upstreams, so both spellings are read.
type catalogEntry struct {
	ID               string `json:"id"`
	Object           string `json:"object"`
	OwnedBy          string `json:"owned_by"`
	ContextLength    int    `json:"context_length"`
	ContextLengthAlt int    `json:"contextWindow"`
	MaxInput         int    `json:"max_input_tokens"`
	Type             string `json:"type"`
}

type catalogEnvelope struct {
	Data []catalogEntry `json:"data"`
}

// Models returns the account's catalog, refreshing it when the TTL has expired.
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	if models, ok := c.cachedCatalog(accountID); ok {
		return models, nil
	}
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	models, err := c.fetchCatalog(ctx, accountID, credential)
	if err != nil {
		// A relay with no usable /models is still usable when the operator
		// listed the models by hand — that is the whole reason the field exists.
		if len(credential.Models) > 0 || len(credential.ModelMapping) > 0 {
			return manualCatalog(credential), nil
		}
		return nil, err
	}
	c.storeCatalog(accountID, models)
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

// fetchCatalog reads the upstream model list and folds in the credential's
// mapping, so a renamed model is requestable before the upstream ever hears
// about it.
func (c *Client) fetchCatalog(ctx context.Context, accountID string, credential Credential) ([]providers.ModelInfo, error) {
	base, err := credential.ChatBase()
	if err != nil {
		return nil, err
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+ModelsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, &providers.Error{
			Kind:    "unavailable",
			Message: "自定义上游不可达：" + err.Error(),
			Err:     err,
		}
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, newProviderError(resp.StatusCode, string(raw))
	}
	if readErr != nil {
		return nil, &providers.Error{Kind: "unavailable", Message: "读取上游模型列表失败：" + readErr.Error(), Err: readErr}
	}
	models, err := ParseCatalogJSON(raw)
	if err != nil {
		return nil, err
	}
	return mergeMapping(models, credential), nil
}

// ParseCatalogJSON maps an OpenAI-shaped /models payload onto ModelInfo. Public
// and native ids stay equal: the point of a relay passthrough is that the client
// asks for the id the relay published.
func ParseCatalogJSON(raw []byte) ([]providers.ModelInfo, error) {
	var envelope catalogEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &providers.Error{Kind: "unavailable", Message: "上游模型列表不是合法 JSON：" + err.Error()}
	}
	if len(envelope.Data) == 0 {
		return nil, &providers.Error{Kind: "unavailable", Message: "上游模型列表为空"}
	}
	models := make([]providers.ModelInfo, 0, len(envelope.Data))
	seen := map[string]bool{}
	for _, entry := range envelope.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || strings.EqualFold(strings.TrimSpace(entry.Type), "image") {
			continue
		}
		if seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		models = append(models, providers.ModelInfo{
			NativeModel: id,
			PublicModel: id,
			DisplayName: id,
			Capabilities: providers.ModelCapabilities{
				ContextWindow: firstPositive(entry.ContextLength, entry.ContextLengthAlt, entry.MaxInput),
				// A relay forwards to whatever it fronts; tools are the
				// near-universal case and the upstream is the authority if a
				// particular model disagrees (it answers with its own 400).
				Tools: true,
			},
		})
	}
	if len(models) == 0 {
		return nil, &providers.Error{Kind: "unavailable", Message: "上游模型列表里没有可用的对话模型"}
	}
	return models, nil
}

// mergeMapping adds `model_mapping` keys as extra public models.
func mergeMapping(models []providers.ModelInfo, credential Credential) []providers.ModelInfo {
	if len(credential.ModelMapping) == 0 {
		return models
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		seen[strings.ToLower(model.PublicModel)] = true
	}
	alias := make([]providers.ModelInfo, 0, len(credential.ModelMapping))
	for public, native := range credential.ModelMapping {
		if seen[strings.ToLower(public)] {
			continue
		}
		alias = append(alias, providers.ModelInfo{
			NativeModel:  native,
			PublicModel:  public,
			DisplayName:  public + " → " + native,
			Capabilities: providers.ModelCapabilities{Tools: true},
		})
	}
	sort.Slice(alias, func(i, j int) bool { return alias[i].PublicModel < alias[j].PublicModel })
	return append(models, alias...)
}

// manualCatalog is the hand-written fallback when /models cannot be read.
func manualCatalog(credential Credential) []providers.ModelInfo {
	models := make([]providers.ModelInfo, 0, len(credential.Models)+len(credential.ModelMapping))
	for _, id := range credential.Models {
		models = append(models, providers.ModelInfo{
			NativeModel:  id,
			PublicModel:  id,
			DisplayName:  id,
			Capabilities: providers.ModelCapabilities{Tools: true},
		})
	}
	return mergeMapping(models, credential)
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// upstreamModel resolves a requested public model to the name the relay expects.
func upstreamModel(model string, credential Credential) string {
	clean := strings.TrimSpace(model)
	clean = strings.TrimPrefix(clean, ProviderID+"/")
	if mapped := strings.TrimSpace(credential.ModelMapping[clean]); mapped != "" {
		return mapped
	}
	return strings.TrimSpace(clean)
}

func newProviderError(status int, body string) *providers.Error {
	classified := Classify(status, body)
	message := classified.Message
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("自定义上游返回 %d", status)
	}
	return &providers.Error{Kind: classified.Kind, Status: classified.Status, Message: message}
}
