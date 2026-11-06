package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Catalog. The gateway's GET /models is OpenAI-shaped:
//
//	{"data":[{"id":"qwen3.8-flash","name":"qwen 3.8 flash (x0.8)",
//	          "multiplier":0.8,"context_length":1000000,
//	          "modalities":["text","image","video"],"owned_by":"loomy"}]}
//
// `multiplier` is the per-request credit cost, which the console renders as the
// model's rate. The list also contains image-generation models
// (Hy-Image-3.5-preview, doubao-seedream-5-lite, qwen-image-3.0-pro) that this
// gateway cannot serve — they need /v1/images/generations — so they are dropped
// instead of advertised and then failing with 404.

// catalogEntry is one model record.
type catalogEntry struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Multiplier       *float64 `json:"multiplier"`
	ContextLength    int      `json:"context_length"`
	Modalities       []string `json:"modalities"`
	Object           string   `json:"object"`
	OwnedBy          string   `json:"owned_by"`
	ContextLengthAlt int      `json:"contextLength"`
}

// imageOnlyModalities marks a record as an image-generation model.
func (entry catalogEntry) imageModel() bool {
	text := false
	for _, modality := range entry.Modalities {
		switch strings.ToLower(strings.TrimSpace(modality)) {
		case "text":
			text = true
		}
	}
	if len(entry.Modalities) == 0 {
		return false
	}
	// The generation models advertise text+image but carry a zero multiplier
	// and no usable context; the reliable signal is the id.
	switch strings.ToLower(entry.ID) {
	case "hy-image-3.5-preview", "doubao-seedream-5-lite", "qwen-image-3.0-pro":
		return true
	}
	return !text
}

func (entry catalogEntry) supportsImages() bool {
	for _, modality := range entry.Modalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

func (entry catalogEntry) contextWindow() int {
	if entry.ContextLength > 0 {
		return entry.ContextLength
	}
	return entry.ContextLengthAlt
}

func (entry catalogEntry) credits() string {
	if entry.Multiplier == nil || *entry.Multiplier <= 0 {
		return ""
	}
	return "x" + strconv.FormatFloat(*entry.Multiplier, 'f', -1, 64) + " credits"
}

func (entry catalogEntry) isFree() bool {
	return entry.Multiplier == nil || *entry.Multiplier == 0
}

func (entry catalogEntry) displayName() string {
	if strings.TrimSpace(entry.Name) != "" {
		return strings.TrimSpace(entry.Name)
	}
	return entry.ID
}

// fallbackModels is the catalog served when /models cannot be read. It mirrors
// the client's own fallback table, minus the generation models.
func fallbackModels() []providers.ModelInfo {
	entries := []struct {
		id, name   string
		mult       float64
		context    int
		modalities []string
	}{
		{"spark-x", "Spark X2.5 (x0.1)", 0.1, 1048576, []string{"text"}},
		{"GLM-5.3-Flash", "GLM 5.3 Flash (x0.8)", 0.8, 1048576, []string{"text", "image", "video"}},
		{"qwen3.8-flash", "qwen 3.8 flash (x0.8)", 0.8, 1000000, []string{"text", "image", "video"}},
		{"deepseek-v4-flash-0731", "DeepSeek V4 Flash 0731 (x3.0)", 3.0, 1048576, []string{"text"}},
		{"mimo-v2.5", "MiMo V2.5 (x3.3)", 3.3, 1048576, []string{"text", "image", "audio", "video"}},
		{"MiniMax-M3", "MiniMax M3 (x4.0)", 4.0, 1048576, []string{"text", "image", "video"}},
		{"Kimi-k2.6", "Kimi k2.6 (x6.5)", 6.5, 262144, []string{"text", "image", "video"}},
		{"qwen-3.8-max", "Qwen 3.8 Max (x12.0)", 12.0, 1000000, []string{"text"}},
	}
	models := make([]providers.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		multiplier := entry.mult
		record := catalogEntry{
			ID: entry.id, Name: entry.name, Multiplier: &multiplier,
			ContextLength: entry.context, Modalities: entry.modalities,
		}
		models = append(models, record.info())
	}
	return models
}

// info converts one gateway record into the shared model shape.
func (entry catalogEntry) info() providers.ModelInfo {
	return providers.ModelInfo{
		NativeModel: entry.ID,
		PublicModel: publicModel(entry.ID),
		DisplayName: entry.displayName(),
		Credits:     entry.credits(),
		Free:        entry.isFree(),
		Capabilities: providers.ModelCapabilities{
			ContextWindow: entry.contextWindow(),
			// The gateway was measured serving tool calls and vision input;
			// it exposes no prompt/output ceilings, so those stay zero rather
			// than being invented.
			Tools:  true,
			Images: entry.supportsImages(),
		},
	}
}

// fetchCatalog reads /models with the account's session.
func (c *Client) fetchCatalog(ctx context.Context, accountID string, cred Credential) ([]providers.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	endpoint := strings.TrimRight(c.modelBase, "/") + modelsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range modelHeaders(cred.Session) {
		req.Header.Set(key, value)
	}
	client, err := c.modelClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &providers.Error{Kind: "unavailable", Message: "loomy /models 不可达：" + err.Error()}
	}
	raw, err := readGatewayJSON(resp)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Data []catalogEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("loomy /models: malformed response: %w", err)
	}
	models := make([]providers.ModelInfo, 0, len(payload.Data))
	seen := map[string]bool{}
	for _, entry := range payload.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || entry.imageModel() || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, entry.info())
	}
	if len(models) == 0 {
		return fallbackModels(), nil
	}
	return models, nil
}

// Models returns the account's catalog: cached while fresh, otherwise fetched,
// otherwise the static fallback (so a transient /models failure never empties a
// channel that can still serve chat).
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	c.mu.Lock()
	cached, ok := c.catalogs[accountID]
	c.mu.Unlock()
	if ok && time.Since(cached.at) < catalogCacheTTL {
		return cached.models, nil
	}
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return fallbackModels(), nil
	}
	models, err := c.fetchCatalog(ctx, accountID, cred)
	if err != nil {
		if ok {
			return cached.models, nil
		}
		return fallbackModels(), nil
	}
	c.mu.Lock()
	if c.catalogs == nil {
		c.catalogs = map[string]catalogCache{}
	}
	c.catalogs[accountID] = catalogCache{models: models, at: time.Now()}
	c.mu.Unlock()
	return models, nil
}
