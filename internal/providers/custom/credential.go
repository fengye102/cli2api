package custom

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Credential is the canonical storage payload for provider=custom.
//
// BaseURL and APIKey are the whole point: they describe one relay account. The
// optional fields cover the cases a pure passthrough cannot know:
//
//   - Name is a human label for the account list ("某中转站-主号").
//   - Models is a manual catalog for relays whose /models is absent or partial.
//   - ModelMapping maps a public name onto a different upstream name, which is
//     how one relay's `gpt-4o` can be exposed as `relay-gpt4o` without editing
//     the model on the upstream side.
type Credential struct {
	Format       string            `json:"format,omitempty"`
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key"`
	Name         string            `json:"name,omitempty"`
	Models       []string          `json:"models,omitempty"`
	ModelMapping map[string]string `json:"model_mapping,omitempty"`
}

// FormatAPIKey trims a pasted key and an accidental Bearer prefix.
func FormatAPIKey(raw string) string {
	key := strings.TrimSpace(raw)
	for _, prefix := range []string{"Bearer ", "bearer ", "BEARER "} {
		key = strings.TrimPrefix(key, prefix)
	}
	return strings.TrimSpace(key)
}

// NormalizeBaseURL turns whatever the operator pasted into an origin plus path
// that ChatPath and ModelsPath can be appended to.
//
// Accepted: "https://relay.example.com", "https://relay.example.com/",
// "https://relay.example.com/v1", "https://relay.example.com/openai/v1".
// Rejected: a URL with no scheme, or a scheme other than http/https — both are
// almost always a paste of the wrong thing and would otherwise fail later as an
// opaque dial error.
func NormalizeBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("base_url 不能为空")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("base_url 不是合法 URL：%w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	case "":
		return "", fmt.Errorf("base_url 缺少协议：应以 http:// 或 https:// 开头")
	default:
		return "", fmt.Errorf("base_url 协议不支持：%s", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("base_url 缺少主机名")
	}
	// The one host worth refusing: pointing an account at the cloud metadata
	// address makes the gateway a credential-exfiltration proxy. Private ranges
	// are allowed on purpose — running a local relay (one-api/new-api) on the
	// same host or LAN is a normal setup.
	if parsed.Hostname() == "169.254.169.254" {
		return "", fmt.Errorf("base_url 不允许指向云元数据地址")
	}
	parsed.Fragment = ""
	parsed.RawQuery = ""
	path := strings.TrimRight(parsed.Path, "/")
	if path == "" {
		path = DefaultBasePath
	}
	parsed.Path = path
	return strings.TrimRight(parsed.String(), "/"), nil
}

// ChatBase is the normalised base URL for this credential.
func (c Credential) ChatBase() (string, error) {
	return NormalizeBaseURL(c.BaseURL)
}

// Ready reports whether the credential can serve a request.
func (c Credential) Ready() bool {
	return strings.TrimSpace(c.APIKey) != "" && strings.TrimSpace(c.BaseURL) != ""
}

// Label is the account label shown in the console.
func (c Credential) Label() string {
	if label := strings.TrimSpace(c.Name); label != "" {
		return label
	}
	base, err := c.ChatBase()
	if err != nil {
		return strings.TrimSpace(c.BaseURL)
	}
	return base
}

// DecodeCredential accepts the canonical form plus the aliases people paste
// from relay dashboards, so {"apiKey":…} or {"baseUrl":…} imports unchanged.
func DecodeCredential(payload []byte) (Credential, error) {
	var alias struct {
		Format       string          `json:"format"`
		BaseURL      string          `json:"base_url"`
		BaseURLCamel string          `json:"baseUrl"`
		URL          string          `json:"url"`
		Base         string          `json:"base"`
		APIBase      string          `json:"api_base"`
		APIKey       string          `json:"api_key"`
		APIKeyCamel  string          `json:"apiKey"`
		Key          string          `json:"key"`
		Token        string          `json:"token"`
		APIToken     string          `json:"api_token"`
		Name         string          `json:"name"`
		Models       json.RawMessage `json:"models"`
		ModelMapping json.RawMessage `json:"model_mapping"`
		Mapping      json.RawMessage `json:"mapping"`
	}
	if err := json.Unmarshal(payload, &alias); err != nil {
		return Credential{}, fmt.Errorf("凭证不是合法 JSON：%w", err)
	}
	credential := Credential{
		Format:  firstNonEmpty(alias.Format, CredentialFormat),
		BaseURL: firstNonEmpty(alias.BaseURL, alias.BaseURLCamel, alias.URL, alias.Base, alias.APIBase),
		APIKey:  FormatAPIKey(firstNonEmpty(alias.APIKey, alias.APIKeyCamel, alias.Key, alias.Token, alias.APIToken)),
		Name:    strings.TrimSpace(alias.Name),
	}
	// The console's single "paste token" box (and any CLI one-liner) hands the
	// whole string over as api_key. A relay needs two values, so accept the
	// compact form there too: "https://relay/v1|sk-...".
	if strings.TrimSpace(credential.BaseURL) == "" {
		if base, key, ok := splitCompactCredential(credential.APIKey); ok {
			credential.BaseURL, credential.APIKey = base, key
		}
	}
	models, err := decodeModels(alias.Models)
	if err != nil {
		return Credential{}, err
	}
	credential.Models = models
	mappingRaw := alias.ModelMapping
	if len(strings.TrimSpace(string(mappingRaw))) == 0 {
		mappingRaw = alias.Mapping
	}
	mapping, err := decodeMapping(mappingRaw)
	if err != nil {
		return Credential{}, err
	}
	credential.ModelMapping = mapping
	return credential, nil
}

// splitCompactCredential splits "https://relay/v1|sk-..." (or the same with a
// space) into a base URL and a key. It only fires when the first half is a URL
// with a scheme, so a key that merely contains a pipe is left alone.
func splitCompactCredential(raw string) (string, string, bool) {
	value := strings.TrimSpace(raw)
	for _, separator := range []string{"|", " ", "\n"} {
		index := strings.Index(value, separator)
		if index <= 0 {
			continue
		}
		base := strings.TrimSpace(value[:index])
		key := FormatAPIKey(value[index+len(separator):])
		if !strings.Contains(base, "://") || key == "" {
			continue
		}
		if _, err := NormalizeBaseURL(base); err != nil {
			continue
		}
		return base, key, true
	}
	return "", "", false
}

// decodeModels accepts ["a","b"] or "a,b".
func decodeModels(raw json.RawMessage) ([]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return cleanModelList(list), nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, fmt.Errorf("models 应为字符串数组或用逗号分隔的字符串")
	}
	return cleanModelList(strings.Split(text, ",")), nil
}

func cleanModelList(items []string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		value := strings.TrimSpace(item)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func decodeMapping(raw json.RawMessage) (map[string]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var mapping map[string]string
	if err := json.Unmarshal(raw, &mapping); err != nil {
		return nil, fmt.Errorf("model_mapping 应为 {\"对外名\":\"上游名\"} 形式")
	}
	out := map[string]string{}
	for key, value := range mapping {
		public := strings.TrimSpace(key)
		native := strings.TrimSpace(value)
		if public == "" || native == "" {
			continue
		}
		out[public] = native
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Encode returns the canonical payload stored in the database.
func (c Credential) Encode() ([]byte, error) {
	c.Format = firstNonEmpty(c.Format, CredentialFormat)
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.APIKey = FormatAPIKey(c.APIKey)
	c.Models = cleanModelList(c.Models)
	return json.Marshal(c)
}

// ValidateCredential is the codec entry point: it must reject a payload that
// could never serve a request, so a broken account is refused at paste time
// instead of failing on first use.
func ValidateCredential(payload []byte) error {
	credential, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(credential.APIKey) == "" {
		return fmt.Errorf("缺少 api_key")
	}
	if _, err := credential.ChatBase(); err != nil {
		return err
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
