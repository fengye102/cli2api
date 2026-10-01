package providers

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Tolerant helpers for credential decoders.
//
// Console users paste raw client-file contents (Codex auth.json, Trae
// storage.json, CodeBuddy state.vscdb values). Those payloads nest tokens
// under provider-specific keys and often serialize whole objects as JSON
// strings, so decoders share the walk helpers below instead of duplicating
// reflection.
//
// Every helper here resolves a payload the same way on every call: object
// keys are walked in sorted order, and field-name lists are honoured in the
// caller's order. Go randomizes map iteration, so a decoder that picked
// "the first match" used to return a different account each time the same
// blob carried several candidates.

// UnwrapJSONValue parses raw into a generic value, unwrapping any level of
// JSON-encoded string content the way Trae storage.json and state.vscdb
// values do.
func UnwrapJSONValue(raw []byte) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return unwrapJSONString(v), nil
}

// unwrapJSONString replaces any string that is itself a JSON object/array
// with its parsed form, recursively.
func unwrapJSONString(v any) any {
	switch t := v.(type) {
	case string:
		trimmed := strings.TrimSpace(t)
		if len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
			return t
		}
		var inner any
		if err := json.Unmarshal([]byte(trimmed), &inner); err != nil {
			return t
		}
		return unwrapJSONString(inner)
	case map[string]any:
		for k, val := range t {
			t[k] = unwrapJSONString(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = unwrapJSONString(val)
		}
		return t
	default:
		return t
	}
}

// DeepPickString walks obj pre-order and returns the first non-empty string
// field whose key matches names (case-insensitive). names are tried in the
// order given, so callers express preference by ordering their list.
func DeepPickString(obj any, names ...string) string {
	var out string
	walkObjects(obj, func(m map[string]any) bool {
		if s, ok := pickString(m, names); ok {
			out = s
			return false
		}
		return true
	})
	return out
}

// DeepPickInt is DeepPickString for numeric fields (or numeric strings).
func DeepPickInt(obj any, names ...string) int64 {
	var out int64
	walkObjects(obj, func(m map[string]any) bool {
		if n, ok := pickInt(m, names); ok {
			out = n
			return false
		}
		return true
	})
	return out
}

// DeepFindAuthObject returns the first object under obj that carries a token
// field (access_token / accessToken / refreshToken / id_token). Pre-order,
// so the outermost auth blob wins over nested duplicates.
func DeepFindAuthObject(obj any) map[string]any {
	var found map[string]any
	walkObjects(obj, func(m map[string]any) bool {
		for _, key := range []string{
			"accessToken", "access_token", "refreshToken", "refresh_token", "RefreshToken", "id_token", "token",
		} {
			if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
				found = m
				return false
			}
		}
		return true
	})
	return found
}

// pickString resolves one string field out of names, preferring an exact key
// and falling back to a case-insensitive match; keys are scanned in sorted
// order so the result never depends on Go's map iteration order.
func pickString(m map[string]any, names []string) (string, bool) {
	for _, name := range names {
		if s, ok := m[name].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), true
		}
	}
	for _, name := range names {
		for _, key := range sortedKeys(m) {
			if !strings.EqualFold(key, name) {
				continue
			}
			if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), true
			}
		}
	}
	return "", false
}

// pickInt is pickString for numbers and numeric strings; names decide the
// preference, so the result does not depend on Go's map order.
func pickInt(m map[string]any, names []string) (int64, bool) {
	read := func(v any) (int64, bool) {
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case string:
			var parsed int64
			if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &parsed); err == nil {
				return parsed, true
			}
		}
		return 0, false
	}
	for _, name := range names {
		if v, ok := m[name]; ok {
			if n, ok := read(v); ok {
				return n, true
			}
		}
	}
	for _, name := range names {
		for _, key := range sortedKeys(m) {
			if !strings.EqualFold(key, name) {
				continue
			}
			if n, ok := read(m[key]); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// walkObjects visits every JSON object pre-order; returning false from fn
// stops the walk. Child keys are visited in sorted order so that a payload
// holding several candidates always resolves to the same one.
func walkObjects(v any, fn func(map[string]any) bool) bool {
	switch t := v.(type) {
	case map[string]any:
		if !fn(t) {
			return false
		}
		for _, key := range sortedKeys(t) {
			if !walkObjects(t[key], fn) {
				return false
			}
		}
	case []any:
		for _, val := range t {
			if !walkObjects(val, fn) {
				return false
			}
		}
	}
	return true
}

// sortedKeys lists an object's keys in a stable order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
