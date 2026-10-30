package autoclaw

import (
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Model routing needs TWO identifiers, and both must be right:
//
//   - X-Request-Model header: the full route id WITH the channel prefix
//     (zai_auto, zaicoding_glm-5.3, zai_glm-5.3-flash). The prefix selects the
//     billing channel: zai_ -> membership/pay-as-you-go, zaicoding_ -> Coding
//     Plan, zai_auto -> let the server pick.
//   - body.model: the model id WITH the prefix stripped (glm-5.3), which is
//     what the client's Model Broker sends.
//
// Send only one of the two correctly and the upstream either bills the wrong
// channel or fails to recognize the model.

// routePrefixes are tried in order when stripping: zaicoding_ before zai_,
// otherwise "zaicoding_glm-5.3" would be stripped to "coding_glm-5.3".
var routePrefixes = []string{"zaicoding_", "zai_"}

// modelEntry is one row of the static catalog.
type modelEntry struct {
	// ID is the catalog id and the name clients usually send: auto, glm-5.3.
	ID string
	// RouteID is the full prefixed route id.
	RouteID string
	// Name is the display name: Auto, GLM-5.3.
	Name string
	// Reasoning / Images / ContextWindow / MaxTokens are capability bits.
	Reasoning     bool
	Images        bool
	ContextWindow int
	MaxTokens     int
}

// models is the static routing table.
//
// The source proxy carried 9 entries from the 1.17.8 settings page, but the
// actual client model picker currently offers only GLM-5.3 and
// GLM-5.3-Flash - the rest were retired in-app. The gateway should not
// advertise more than the real client can select.
//
// Order is the /v1/models display order.
var models = []modelEntry{
	{
		ID: "glm-5.3", RouteID: "zaicoding_glm-5.3", Name: "GLM-5.3",
		Reasoning: true, Images: false,
		ContextWindow: 1048576, MaxTokens: 307200,
	},
	{
		ID: "glm-5.3-flash", RouteID: "zai_glm-5.3-flash", Name: "GLM-5.3-Flash",
		Reasoning: true, Images: true,
		ContextWindow: 1048576, MaxTokens: 131072,
	},
}

// modelRoute is the resolved routing decision for one request.
type modelRoute struct {
	// RouteModelID goes into the X-Request-Model header.
	RouteModelID string
	// BodyModelID goes into body.model.
	BodyModelID string
	// RequestedModel is what the client asked for, kept for the response
	// rewrite and logs.
	RequestedModel string
}

// stripRoutePrefix removes a known channel prefix.
func stripRoutePrefix(routeID string) string {
	for _, prefix := range routePrefixes {
		if rest, ok := strings.CutPrefix(routeID, prefix); ok {
			return rest
		}
	}
	return routeID
}

// isRouteID reports whether value has the legal route-id shape:
// a lowercase letter, then lowercase letters/digits, then an underscore,
// then 1..127 characters from [A-Za-z0-9._:-].
func isRouteID(value string) bool {
	if value == "" {
		return false
	}
	if value[0] < 'a' || value[0] > 'z' {
		return false
	}
	underscore := -1
	for i := 1; i < len(value); i++ {
		ch := value[i]
		if ch == '_' {
			underscore = i
			break
		}
		if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') {
			return false
		}
	}
	if underscore < 0 {
		return false
	}
	tail := value[underscore+1:]
	if tail == "" || len(tail) > 127 {
		return false
	}
	for i := 0; i < len(tail); i++ {
		ch := tail[i]
		ok := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == ':' || ch == '-'
		if !ok {
			return false
		}
	}
	return true
}

// entryByCatalogName matches a catalog row the same way the aggregate catalog
// does: id first, then display name, then full route id, all trimmed and
// ASCII-case-insensitive.
//
// Matching the catalog's own rules matters: the aggregate layer lets "GLM-5.3"
// and "Auto" through, so this layer must resolve them to the canonical route.
// A case-sensitive match here would silently fall back to zai_auto - picking
// the wrong model AND the wrong billing channel.
func entryByCatalogName(name string) (modelEntry, bool) {
	needle := strings.TrimSpace(name)
	for _, entry := range models {
		if strings.EqualFold(entry.ID, needle) {
			return entry, true
		}
	}
	for _, entry := range models {
		if strings.EqualFold(entry.Name, needle) {
			return entry, true
		}
	}
	for _, entry := range models {
		if strings.EqualFold(entry.RouteID, needle) {
			return entry, true
		}
	}
	return modelEntry{}, false
}

// resolveModelRoute maps a client model name to the upstream route.
//
// Three accepted spellings: a catalog entry (glm-5.3, GLM-5.3,
// zaicoding_glm-5.3), an explicit route prefix, or any other legal route id
// (x_yyy) passed through. Anything else falls back to the default route,
// which is what keeps Anthropic clients sending claude-* working.
//
// The fallback never rewrites case: it exists to catch genuinely unknown names,
// not to paper over a case mismatch.
func resolveModelRoute(region Region, requested string) modelRoute {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		return modelRoute{
			RouteModelID:   defaultRoute,
			BodyModelID:    stripRoutePrefix(defaultRoute),
			RequestedModel: "",
		}
	}
	if entry, ok := entryByCatalogName(trimmed); ok {
		return modelRoute{
			RouteModelID:   entry.RouteID,
			BodyModelID:    stripRoutePrefix(entry.RouteID),
			RequestedModel: requested,
		}
	}
	// Explicit route spelling: keep it verbatim, strip the prefix for the body.
	if isRouteID(trimmed) {
		return modelRoute{
			RouteModelID:   trimmed,
			BodyModelID:    stripRoutePrefix(trimmed),
			RequestedModel: requested,
		}
	}
	return modelRoute{
		RouteModelID:   defaultRoute,
		BodyModelID:    stripRoutePrefix(defaultRoute),
		RequestedModel: requested,
	}
}

// staticModels is the built-in catalog advertised when the remote one is
// unavailable.
func staticModels() []providers.ModelInfo {
	out := make([]providers.ModelInfo, 0, len(models))
	for _, entry := range models {
		out = append(out, providers.ModelInfo{
			NativeModel: entry.ID,
			PublicModel: entry.ID,
			DisplayName: entry.Name,
			Capabilities: providers.ModelCapabilities{
				ContextWindow: entry.ContextWindow,
				MaxOutput:     entry.MaxTokens,
				Tools:         true,
				Images:        entry.Images,
				Reasoning:     entry.Reasoning,
			},
		})
	}
	return out
}
