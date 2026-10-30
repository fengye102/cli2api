package autoclaw

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	proxyutil "github.com/caigee-cmd/cli2api/internal/proxy"
)

// Store is the persistence surface this adapter needs. It never imports
// internal/store.
type Store interface {
	Get(ctx context.Context, id string) (accounts.Account, error)
	LoadCredentialPayload(ctx context.Context, accountID string) (string, []byte, error)
	SaveCredentialPayload(ctx context.Context, accountID, format string, payload []byte) error
	Observe(ctx context.Context, id, remoteUID, status, lastError, lastKind string) error
}

// SecretReader is optional. Missing it means no global proxy.
type SecretReader interface {
	GetSecret(context.Context, string) (string, bool, error)
}

// Client is the in-process AutoClaw adapter for one region. Credentials and
// catalogs stay keyed by account id, so one client serves every account of its
// region.
type Client struct {
	store  Store
	region Region
	http   *http.Client

	transports proxyutil.TransportCache

	// upstreamBase / userapiBase overrides for tests.
	upstreamBase string
	userapiBase  string

	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	catalogs map[string]catalogCache

	// forcedRefresh tracks the last hourly maintenance refresh per account, so
	// a long-idle refresh token does not get rotated away from under us.
	refreshMu sync.Mutex
	refreshed map[string]time.Time

	nowFn func() time.Time
}

type catalogCache struct {
	models []providers.ModelInfo
	at     time.Time
}

// NewClient builds the adapter for one region.
func NewClient(store Store, region Region) *Client {
	// Bound the dial: a blocked egress used to burn kernel TCP retries and
	// surface as a silent multi-minute hang, which the console shows as a
	// request that never returns.
	dialer := &net.Dialer{Timeout: dialTimeoutSeconds * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         dialer.DialContext,
	}
	return &Client{
		store:  store,
		region: region,
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		upstreamBase: region.upstreamBase(),
		userapiBase:  region.userapiBase(),
		locks:        map[string]*sync.Mutex{},
		catalogs:     map[string]catalogCache{},
		refreshed:    map[string]time.Time{},
		nowFn:        time.Now,
	}
}

func (c *Client) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

// accountLock serializes refresh per account so a burst of 401s cannot burn
// the single-use refresh token more than once.
func (c *Client) accountLock(accountID string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	lock, ok := c.locks[accountID]
	if !ok {
		lock = &sync.Mutex{}
		c.locks[accountID] = lock
	}
	return lock
}

func (c *Client) globalProxy(ctx context.Context) (string, error) {
	store, ok := c.store.(SecretReader)
	if !ok {
		return "", nil
	}
	value, found, err := store.GetSecret(ctx, "proxy_url")
	if err != nil {
		return "", fmt.Errorf("load global proxy setting: %w", err)
	}
	if !found {
		return "", nil
	}
	return strings.TrimSpace(value), nil
}

// effectiveProxy resolves the egress for one account: account proxy first,
// then the global setting.
func (c *Client) effectiveProxy(ctx context.Context, accountID string) (string, error) {
	if strings.TrimSpace(accountID) == "" {
		return c.globalProxy(ctx)
	}
	account, err := c.store.Get(ctx, accountID)
	if err != nil {
		return "", err
	}
	if value := strings.TrimSpace(account.ProxyURL); value != "" {
		return value, nil
	}
	return c.globalProxy(ctx)
}

// httpClient returns a client bound to the account's egress.
//
// Every call that touches the network passes through here, including the
// refresh and identity probes: an account-scoped proxy_url that only applied to
// chat would leave token exchange on a blocked direct route, which is exactly
// how a "proxied" account ends up hanging on login.
func (c *Client) httpClient(ctx context.Context, accountID string) (*http.Client, error) {
	rawProxy, err := c.effectiveProxy(ctx, accountID)
	if err != nil {
		return nil, err
	}
	client := *c.http
	transport, err := c.transports.Get(rawProxy)
	if err != nil {
		return nil, err
	}
	if transport != nil {
		client.Transport = transport
	}
	return &client, nil
}

// signHeaders builds the X-Auth-Sign fingerprint used on every userapi call
// (refresh, balance, catalog).
//
// The timestamp is in SECONDS - a millisecond value silently fails the
// signature check (code 400002) and only shows up as a fallback-refresh.
//
// X-Harness-Type IS sent here and deliberately is NOT sent on the chat path:
// the upstream treats "zcode" differently on /chat/completions (403 pay-view /
// 406) but accepts it on userapi, where it is part of the official client
// fingerprint. Do not unify the two header sets.
func signedHeaders(token string, now time.Time) map[string]string {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	sum := md5.Sum([]byte(authAppID + "&" + timestamp + "&" + authAppKey))
	headers := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "*/*",
		"User-Agent":       clientUserAgent,
		"X-Version":        userapiVersion,
		"X-Product":        clientProduct,
		"X-Client-Type":    clientType,
		"X-Harness-Type":   harnessType,
		"X-Tm":             "win",
		"X-Lang":           clientLang,
		"X-Channel":        clientChannel,
		"X-Auth-Appid":     authAppID,
		"X-Auth-TimeStamp": timestamp,
		"X-Auth-Sign":      hex.EncodeToString(sum[:]),
	}
	if token != "" {
		headers["authorization"] = "Bearer " + token
	}
	return headers
}

// clientUserAgent is sent on every outbound request.
//
// This is NOT cosmetic. The upstream sits behind an Aliyun WAF that rejects
// Go's default "Go-http-client/1.1" User-Agent with a 405 HTML page before the
// request ever reaches the autoclaw gateway - the response looks like a
// routing bug (HTML, not JSON) and never carries the real error. Measured
// 2026-10-30 from the LA host, same token/body/headers, UA varied alone:
//
//	Go-http-client/1.1   -> 405  (WAF block, HTML body)
//	curl/8.5.0           -> 200
//	Mozilla ... Chrome   -> 403  (WAF rejects a browser UA on this endpoint)
//	Electron/32.2.7 ...  -> 200
//	autoclaw/1.20.0      -> 200
//
// Electron is the honest choice: AutoClaw is an Electron desktop app, so this
// is the UA the real client presents, and it is one of the values the WAF
// accepts.
const clientUserAgent = "Electron/32.2.7 Chrome/128.0.6613.186"

// brandHeaders is the chat-path client fingerprint. X-Harness-Type is absent
// on purpose (see signedHeaders).
func brandHeaders() map[string]string {
	return map[string]string{
		"User-Agent":    clientUserAgent,
		"X-Product":     clientProduct,
		"X-Client-Type": clientType,
		"X-Tm":          "win",
		"X-Version":     desktopAppVersion,
		"X-Lang":        clientLang,
		"X-Channel":     clientChannel,
		"x_trace_id":    clientTraceID,
	}
}

// doJSON performs one signed userapi request and returns the raw body.
//
// The caller supplies the access token because the upstream requires
// `authorization: Bearer <access_token>` on these paths even when the body
// carries a refresh_token: posting /refresh with no Authorization header (or a
// bogus refresh_token) answers `{"code":400000,"msg":"用户未登录，请重新登录"}`
// rather than a refresh-specific error. Passing "" is only correct for paths
// that genuinely need no identity.
func (c *Client) doJSON(ctx context.Context, accountID, token, path string, body any) (json.RawMessage, int, error) {
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, 0, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encode autoclaw request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.userapiBase+path, reader)
	if err != nil {
		return nil, 0, err
	}
	for name, value := range signedHeaders(token, c.now()) {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return json.RawMessage(raw), resp.StatusCode, nil
}

// envelope is the shared userapi response shape.
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func parseEnvelope(raw json.RawMessage) (envelope, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return envelope{}, fmt.Errorf("parse autoclaw response: %w", err)
	}
	return env, nil
}

func (c *Client) truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
