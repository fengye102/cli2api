package zcode

// A ZCode config.json holds a provider table, and since 3.15 that is where the
// coding plan's key lives: provider.<id>.options.apiKey (magpie#2130). The
// table usually holds more than ZCode's own entry — any other coding agent the
// user pointed at a gateway sits beside it, magpie's own entry among them — so
// taking the first apiKey found would import someone else's key, and Go's map
// iteration made "first" different from one import to the next. The entry that
// names a ZCode site wins; the rest are only used when nothing names one.

import (
	"net/url"
	"sort"
	"strings"
)

// localProviderEntry is one provider section of a ZCode config.json.
type localProviderEntry struct {
	ID      string
	APIKey  string
	BaseURL string
}

// localProviderEntries collects the provider sections of a ZCode config.json,
// sorted by id so an import is reproducible.
func localProviderEntries(doc any) []localProviderEntry {
	var entries []localProviderEntry
	walkJSONObjects(doc, func(m map[string]any) bool {
		for _, table := range []string{"provider", "providers"} {
			table, ok := m[table].(map[string]any)
			if !ok {
				continue
			}
			for id, raw := range table {
				section, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				entry := localProviderEntry{ID: id}
				entry.APIKey = firstLocalString(section, "apiKey", "api_key")
				entry.BaseURL = firstLocalString(section, "baseURL", "base_url")
				if options, ok := section["options"].(map[string]any); ok {
					if key := firstLocalString(options, "apiKey", "api_key"); key != "" {
						entry.APIKey = key
					}
					if base := firstLocalString(options, "baseURL", "base_url"); base != "" {
						entry.BaseURL = base
					}
				}
				if entry.APIKey != "" {
					entries = append(entries, entry)
				}
			}
		}
		return true
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

// firstLocalString reads a plaintext string field from one map, skipping sealed
// values.
func firstLocalString(m map[string]any, names ...string) string {
	for _, name := range names {
		if s, ok := m[name].(string); ok {
			if s = strings.TrimSpace(s); s != "" && !isSealed(s) {
				return s
			}
		}
	}
	return ""
}

// zcodeSiteURL reports whether a base URL points at a site ZCode serves: Z.ai
// or BigModel, and nowhere else.
func zcodeSiteURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "z.ai" || strings.HasSuffix(host, ".z.ai") ||
		host == "bigmodel.cn" || strings.HasSuffix(host, ".bigmodel.cn")
}

// pickProviderCredential returns the api key and base URL of the provider
// section that belongs to ZCode. A section naming a ZCode site wins even when
// it sorts later; a document with no provider table at all reports nothing and
// the caller falls back to its own walk.
func pickProviderCredential(doc any) (string, string) {
	entries := localProviderEntries(doc)
	if len(entries) == 0 {
		return "", ""
	}
	for _, entry := range entries {
		if zcodeSiteURL(entry.BaseURL) || strings.Contains(strings.ToLower(entry.ID), "zcode") {
			return entry.APIKey, entry.BaseURL
		}
	}
	// Nothing names a ZCode site: a single entry is unambiguous, and with
	// several the sorted first keeps the choice reproducible.
	return entries[0].APIKey, entries[0].BaseURL
}
