package antigravity

import (
	"strings"
	"time"
)

// catalogTTL bounds how long a fetched model list is reused.
const catalogTTL = 30 * time.Minute

// timeNow is injectable for catalog-cache tests.
var timeNow = time.Now

// endpoint appends an OpenAI path to a base that already carries the
// v1beta/openai version segment, without doubling it.
func endpoint(base, endpoint string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	endpoint = "/" + strings.TrimLeft(strings.TrimSpace(endpoint), "/")
	if strings.HasSuffix(strings.ToLower(base), strings.ToLower(endpoint)) {
		return base
	}
	return base + endpoint
}
