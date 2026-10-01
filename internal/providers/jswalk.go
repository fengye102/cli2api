package providers

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Tolerant helpers for credential decoders.
//
// Console users paste raw client-file contents (Codex auth.json, Trae
// storage.json, CodeBuddy state.vscdb values). Those payloads nest tokens
// under provider-specific keys and often serialize whole objects as JSON
// strings, so decoders share the walk helpers below instead of duplicating
// reflection.

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
// field whose key matches names (case-insensitive).
func DeepPickString(obj any, names ...string) string {
	var out string
	walkObjects(obj, true, func(m map[string]any) bool {
		for k, val := range m {
			s, ok := val.(string)
			if !ok || strings.TrimSpace(s) == "" {
				continue
			}
			for _, name := range names {
				if strings.EqualFold(k, name) {
					out = strings.TrimSpace(s)
					return false
				}
			}
		}
		return true
	})
	return out
}

// DeepPickInt is DeepPickString for numeric fields (or numeric strings).
func DeepPickInt(obj any, names ...string) int64 {
	var out int64
	walkObjects(obj, true, func(m map[string]any) bool {
		for k, val := range m {
			matched := false
			for _, name := range names {
				if strings.EqualFold(k, name) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			switch n := val.(type) {
			case float64:
				out = int64(n)
				return false
			case string:
				var parsed int64
				if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &parsed); err == nil {
					out = parsed
					return false
				}
			}
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
	walkObjects(obj, true, func(m map[string]any) bool {
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

// walkObjects visits every JSON object pre-order; returning false from fn
// stops the walk.
func walkObjects(v any, root bool, fn func(map[string]any) bool) bool {
	init := true
	switch t := v.(type) {
	case map[string]any:
		if !fn(t) {
			return false
		}
		for _, val := range t {
			if !walkObjects(val, false, fn) {
				return false
			}
		}
	case []any:
		for _, val := range t {
			if !walkObjects(val, false, fn) {
				return false
			}
		}
	}
	_ = init
	return true
}
