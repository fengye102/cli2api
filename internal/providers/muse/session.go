package muse

import (
	"encoding/json"
	"strings"
)

// parseSessionSnapshot reads the parts of /api/session this adapter cares
// about. muse.ai's payload is not contractual and changes shape between
// releases, so every lookup is best effort and a parse failure yields an empty
// snapshot instead of an error: the HTTP status already decided readiness.
func parseSessionSnapshot(body []byte) sessionSnapshot {
	var snapshot sessionSnapshot
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		return snapshot
	}
	snapshot.Kind = firstString(generic, "kind", "state", "session_state", "status")

	// Identity: the payload may nest the user under "user"/"viewer"/"account".
	for _, key := range []string{"user", "viewer", "account", "profile", "current_user"} {
		nested, ok := generic[key].(map[string]any)
		if !ok {
			continue
		}
		if snapshot.Email == "" {
			snapshot.Email = firstString(nested, "email", "email_address", "username", "handle")
		}
		if snapshot.UID == "" {
			snapshot.UID = firstString(nested, "id", "user_id", "uid", "userid")
		}
	}
	if snapshot.Email == "" {
		snapshot.Email = firstString(generic, "email", "email_address")
	}
	if snapshot.UID == "" {
		snapshot.UID = firstString(generic, "user_id", "uid", "userid", "id")
	}

	// VM state: hatch runs the agent in a per-user VM.
	for _, key := range []string{"vm", "hatch_vm", "machine"} {
		nested, ok := generic[key].(map[string]any)
		if !ok {
			continue
		}
		if state := firstString(nested, "state", "status"); state != "" {
			snapshot.VMState = state
			break
		}
	}
	if snapshot.VMState == "" {
		snapshot.VMState = firstString(generic, "vm_state", "vm_status")
	}
	return snapshot
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}
