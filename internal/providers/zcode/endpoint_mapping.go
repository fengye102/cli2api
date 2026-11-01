package zcode

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Endpoint mapping.
//
// The desktop client (3.7+) does not hardcode where it sends inference: it
// periodically reads GET {origin}/api/v1/agent/configs and rewrites the request
// URL through the proxyEndpoint.mapping table the answer carries. That is how
// the vendor moves traffic between planes without shipping a client — as of
// 2026-08 the coding-plan Anthropic endpoint is mapped onto
// zcode.z.ai/api/v1/ultra[-zai]/…, and the documentation's "端点由 ZCode 自动路由"
// is this table.
//
// Following it is worth doing for one reason: when the upstream moves the
// endpoint, a gateway that pinned the old one starts failing every request and
// has to wait for a release. Following it must never be able to break a request
// on its own, so everything here is fail-open: no snapshot, no matching entry,
// an unparseable table, a failed fetch — the original URL goes out unchanged.
// apply is a pure table lookup, so the hot path never waits on the network.
//
// Shape of the table (matching the client's own semantics):
//
//   - key = the normalized request URL https://host:port/path with the scheme
//     and host lowercased, an explicit :443 taken as the default, and the path
//     minus its trailing slash;
//   - only https entries count;
//   - the rewrite replaces origin+path and KEEPS the original request's query
//     (a query on the target is dropped);
//   - the table is capped, and a bad entry is skipped rather than failing the
//     whole table.
//
// ZCODE_DISABLE_ENDPOINT_MAPPING turns it off (the fetch stops and the snapshot
// is dropped): if the upstream ever publishes a table that breaks us, an
// operator does not have to wait for a fix.
const (
	agentConfigsPath = "/api/v1/agent/configs"

	// endpointMappingTTL is how long a snapshot is trusted. The client refreshes
	// on the same order of magnitude; a move is not urgent enough to poll
	// harder, and a stale table only matters if the upstream removes an
	// endpoint (which fails open to a real error from that endpoint anyway).
	endpointMappingTTL = 5 * time.Minute
	// endpointMappingMaxEntries bounds the table: it is upstream-controlled
	// data, and an unbounded one is a memory problem waiting to happen.
	endpointMappingMaxEntries = 256
	// endpointMappingTimeout bounds one fetch. It runs off the request path, so
	// it can afford a little patience and still not pile up.
	endpointMappingTimeout = 5 * time.Second
)

// mappingSnapshot is one origin's table plus when it was taken.
type mappingSnapshot struct {
	table map[string]string
	at    time.Time
}

// endpointMappingDisabled reports whether the operator turned the feature off.
func endpointMappingDisabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("ZCODE_DISABLE_ENDPOINT_MAPPING")))
	return value != "" && value != "0" && value != "false"
}

// mapEndpoint rewrites a request URL through the current snapshot.
//
// Pure and synchronous by design: the chat path calls this on every request and
// must never block on the network. Anything unexpected returns the input.
func (c *Client) mapEndpoint(target string) string {
	if c == nil || endpointMappingDisabled() {
		return target
	}
	key, ok := mappingKey(target)
	if !ok {
		return target
	}
	c.mapMu.Lock()
	snapshot, found := c.mappings[mappingOrigin(target)]
	c.mapMu.Unlock()
	if !found {
		return target
	}
	to, hit := snapshot.table[key]
	if !hit {
		return target
	}
	return rewriteKeepingQuery(to, target)
}

// refreshEndpointMapping starts a background fetch when the snapshot for this
// request's origin is missing or stale.
//
// The request itself never waits: a refresh is a goroutine with its own
// deadline, and exactly one runs per origin at a time so a burst of requests
// cannot stampede the upstream.
func (c *Client) refreshEndpointMapping(ctx context.Context, accountID string, cred Credential, target string) {
	if c == nil || endpointMappingDisabled() {
		return
	}
	origin := mappingOrigin(target)
	if origin == "" {
		return
	}
	c.mapMu.Lock()
	if endpointMappingStale(c.mappings[origin]) {
		if c.mappingInFlight == nil {
			c.mappingInFlight = map[string]bool{}
		}
		if c.mappingInFlight[origin] {
			c.mapMu.Unlock()
			return
		}
		c.mappingInFlight[origin] = true
	} else {
		c.mapMu.Unlock()
		return
	}
	c.mapMu.Unlock()

	// The fetch outlives this request (it is a cache fill, not a step in it),
	// so it must not inherit the caller's cancellation — only its values.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endpointMappingTimeout)
	go func() {
		defer cancel()
		defer func() {
			c.mapMu.Lock()
			delete(c.mappingInFlight, origin)
			c.mapMu.Unlock()
		}()
		_ = c.fetchEndpointMapping(fetchCtx, accountID, cred, origin)
	}()
}

// sameMapping reports whether two tables are identical.
func sameMapping(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

// describeMapping renders a table for one log line, truncated.
func describeMapping(table map[string]string) string {
	if len(table) == 0 {
		return ""
	}
	parts := make([]string, 0, len(table))
	for from, to := range table {
		parts = append(parts, from+" -> "+to)
	}
	rendered := strings.Join(parts, "; ")
	if len(rendered) > 400 {
		rendered = rendered[:400] + "…"
	}
	return ": " + rendered
}

// endpointMappingStale reports whether a snapshot needs replacing.
func endpointMappingStale(snapshot mappingSnapshot) bool {
	if snapshot.at.IsZero() {
		return true
	}
	return time.Since(snapshot.at) > endpointMappingTTL
}

// fetchEndpointMapping pulls the table and stores it.
//
// A fetch that fails leaves the previous snapshot alone: not being able to read
// the table is not the same as the upstream having published an empty one.
func (c *Client) fetchEndpointMapping(ctx context.Context, accountID string, cred Credential, origin string) error {
	if endpointMappingDisabled() {
		return nil
	}
	endpoint := origin + agentConfigsPath +
		"?app_version=" + url.QueryEscape(Version) + "&platform=" + url.QueryEscape(platformID())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	for key, value := range defaultHeaders() {
		req.Header.Set(key, value)
	}
	for key, value := range identityHeaders(accountID) {
		req.Header.Set(key, value)
	}
	// The client sends the coding-plan key when it has one; on the plan channel
	// the session token is what it holds instead. Either is worth presenting:
	// the endpoint may be answered anonymously or refuse, and a refusal only
	// leaves the snapshot as it was.
	if key := strings.TrimSpace(cred.APIKey); key != "" {
		req.Header.Set("x-api-key", key)
	}
	if token := strings.TrimSpace(cred.ZCodeJWT); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if token := strings.TrimSpace(cred.AccessToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	table := parseEndpointMapping(body)
	c.mapMu.Lock()
	if c.mappings == nil {
		c.mappings = map[string]mappingSnapshot{}
	}
	previous, had := c.mappings[origin]
	c.mappings[origin] = mappingSnapshot{table: table, at: time.Now()}
	c.mapMu.Unlock()
	// Logged only when the table changes: this is the one event an operator
	// needs to see ("the upstream moved an endpoint, we followed it"), and the
	// fetch itself runs every few minutes.
	if !had || !sameMapping(previous.table, table) {
		log.Printf("zcode endpoint mapping %s: %d entries%s", origin, len(table), describeMapping(table))
	}
	return nil
}

// parseEndpointMapping reads data.proxyEndpoint.mapping out of an
// agent/configs answer. A missing section is an empty table, which is the
// upstream saying it has no mappings right now.
func parseEndpointMapping(payload []byte) map[string]string {
	table := map[string]string{}
	var envelope struct {
		Data struct {
			ProxyEndpoint struct {
				Mapping []struct {
					From string `json:"from"`
					To   string `json:"to"`
				} `json:"mapping"`
			} `json:"proxyEndpoint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return table
	}
	for _, entry := range envelope.Data.ProxyEndpoint.Mapping {
		if len(table) >= endpointMappingMaxEntries {
			break
		}
		key, ok := mappingKey(entry.From)
		if !ok {
			continue
		}
		target, ok := mappingOriginAndPath(entry.To)
		if !ok {
			continue
		}
		table[key] = target
	}
	return table
}

// mappingKey normalizes a URL into the table's lookup key.
func mappingKey(raw string) (string, bool) {
	parsed, ok := parseHTTPS(raw)
	if !ok {
		return "", false
	}
	return "https://" + mappingAuthority(parsed, true) + normalizeMappingPath(parsed.Path), true
}

// mappingOriginAndPath renders a mapping target: origin and path only, since
// the rewrite keeps the caller's query.
func mappingOriginAndPath(raw string) (string, bool) {
	parsed, ok := parseHTTPS(raw)
	if !ok {
		return "", false
	}
	return "https://" + mappingAuthority(parsed, false) + normalizeMappingPath(parsed.Path), true
}

// mappingOrigin is scheme://host:port for snapshot bookkeeping.
func mappingOrigin(raw string) string {
	parsed, ok := parseHTTPS(raw)
	if !ok {
		return ""
	}
	return "https://" + mappingAuthority(parsed, false)
}

// mappingAuthority renders host[:port]; withDefaultPort always states 443 so
// the key is stable whether or not the caller spelled the port out.
func mappingAuthority(parsed *url.URL, withDefaultPort bool) string {
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "" {
		if withDefaultPort {
			return host + ":443"
		}
		return host
	}
	if withDefaultPort || port != "443" {
		return host + ":" + port
	}
	return host
}

// normalizeMappingPath drops trailing slashes, keeping the bare root.
func normalizeMappingPath(path string) string {
	for len(path) > 1 && strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	if path == "" {
		return "/"
	}
	return path
}

// rewriteKeepingQuery swaps origin+path and keeps the original query.
func rewriteKeepingQuery(target, original string) string {
	parsed, err := url.Parse(original)
	if err != nil || parsed.RawQuery == "" {
		return target
	}
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	return target + separator + parsed.RawQuery
}

// parseHTTPS accepts only absolute https URLs with a host: anything else is not
// part of this protocol and is refused rather than guessed at.
func parseHTTPS(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return nil, false
	}
	return parsed, true
}
