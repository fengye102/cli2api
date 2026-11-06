package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// SecretReader is optional. Missing it means no global proxy setting.
type SecretReader interface {
	GetSecret(context.Context, string) (string, bool, error)
}

type catalogCache struct {
	models []providers.ModelInfo
	at     time.Time
}

// loginState is one in-flight device-code round, kept in memory between
// StartLogin and PollLogin. It holds the PKCE verifier, which must never be
// persisted or logged.
type loginState struct {
	site       Site
	device     deviceAuth
	verifier   string
	startedAt  time.Time
	lastPolled time.Time
	lastNote   string
}

// refreshCall is the single-flight slot for one account's token refresh.
type refreshCall struct {
	done chan struct{}
	cred Credential
	err  error
}

// Client is the in-process MiniMax Code adapter. One client serves every
// account; each account carries its own OAuth pair and region.
type Client struct {
	store Store
	http  *http.Client

	transports proxyutil.TransportCache
	now        func() time.Time
	// siteResolver lets a test point the client at a fake gateway. Nil means
	// the real site table.
	siteResolver func(region string) Site

	mu        sync.Mutex
	catalogs  map[string]catalogCache
	logins    map[string]*loginState
	refreshes map[string]*refreshCall
}

func NewClient(store Store) *Client {
	return &Client{
		store: store,
		http: &http.Client{
			// No client timeout: a long turn streams for minutes. Deadlines are
			// per-request (TTFB and ctx), never a whole-turn cap.
			Timeout: 0,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:       time.Now,
		catalogs:  map[string]catalogCache{},
		logins:    map[string]*loginState{},
		refreshes: map[string]*refreshCall{},
	}
}

// site resolves a region onto a deployment, honouring a test override.
func (c *Client) site(region string) Site {
	if c.siteResolver != nil {
		return c.siteResolver(region)
	}
	return siteFor(region)
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
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

// credential loads and decodes the stored payload without touching the network.
func (c *Client) credential(ctx context.Context, accountID string) (Credential, error) {
	_, payload, err := c.store.LoadCredentialPayload(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	cred, err := DecodeCredential(payload)
	if err != nil {
		return Credential{}, err
	}
	if region := regionOfAccount(ctx, c.store, accountID); region != "" && strings.TrimSpace(cred.Region) == "" {
		cred.Region = region
	}
	if !cred.Ready() {
		return cred, fmt.Errorf("minimax 账号凭证不完整：需要 access_token 或 refresh_token")
	}
	return cred, nil
}

// regionOfAccount reads the account's registry region, which is where the
// console records the site the operator picked.
func regionOfAccount(ctx context.Context, store Store, accountID string) string {
	account, err := store.Get(ctx, accountID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(account.ProviderRegion)
}

func (c *Client) saveCredential(ctx context.Context, accountID string, cred Credential) error {
	payload, err := EncodeCredential(cred)
	if err != nil {
		return err
	}
	return c.store.SaveCredentialPayload(ctx, accountID, CredentialFormat, payload)
}

// freshCredential returns a credential whose access token is usable now: it
// refreshes when the deadline is close (or the token is missing) and hands back
// the stored value otherwise.
func (c *Client) freshCredential(ctx context.Context, accountID string) (Credential, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	if !cred.NeedsRefresh(c.clock()) {
		return cred, nil
	}
	return c.refresh(ctx, accountID, cred)
}

// refresh renews an account's access token exactly once, even when several
// chats hit a 401 at the same moment. MiniMax's refresh tokens are single use,
// so the second concurrent caller must wait for the first result instead of
// spending its own copy.
func (c *Client) refresh(ctx context.Context, accountID string, stale Credential) (Credential, error) {
	c.mu.Lock()
	if pending, ok := c.refreshes[accountID]; ok {
		c.mu.Unlock()
		select {
		case <-pending.done:
			return pending.cred, pending.err
		case <-ctx.Done():
			return Credential{}, ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	c.refreshes[accountID] = call
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.refreshes, accountID)
		c.mu.Unlock()
		close(call.done)
	}()

	// Someone else may have renewed between our load and this slot: reuse their
	// result rather than spending a second refresh token.
	if current, err := c.credential(ctx, accountID); err == nil {
		if current.AccessToken != stale.AccessToken && current.AccessToken != "" {
			call.cred = current
			return current, nil
		}
		if !current.CanRefresh() {
			call.err = &providers.Error{Kind: accounts.KindAuth, Status: http.StatusUnauthorized,
				Message: "minimax 登录已失效且没有 refresh_token，请重新登录"}
			return Credential{}, call.err
		}
		stale = current
	}

	site := c.site(stale.Region)
	tokens, err := c.exchangeRefresh(ctx, accountID, site, stale.RefreshToken)
	if err != nil {
		if refusal, ok := err.(*refreshError); ok {
			call.err = &providers.Error{Kind: accounts.KindAuth, Status: http.StatusUnauthorized,
				Message: "minimax 刷新被上游拒绝，请重新登录：" + refusal.Reason, Err: err}
			return Credential{}, call.err
		}
		call.err = &providers.Error{Kind: accounts.KindUnavailable, Status: http.StatusBadGateway,
			Message: "minimax 刷新令牌失败：" + err.Error(), Err: err}
		return Credential{}, call.err
	}
	renewed := Credential{
		Format:       CredentialFormat,
		AccessToken:  tokens.AccessToken,
		RefreshToken: stale.RefreshToken,
		ExpiresAt:    deadlineFrom(tokens.ExpiresIn, c.clock()),
		Region:       site.Region,
		TokenType:    firstNonEmpty(tokens.TokenType, stale.TokenType),
		Subject:      stale.Subject,
	}
	// The vendor rotates the refresh token on every refresh; when it does not
	// send a new one, the old value stays valid and is kept.
	if tokens.RefreshToken != "" {
		renewed.RefreshToken = tokens.RefreshToken
	}
	if err := c.saveCredential(ctx, accountID, renewed); err != nil {
		call.err = fmt.Errorf("minimax save refreshed credential: %w", err)
		return Credential{}, call.err
	}
	call.cred = renewed
	return renewed, nil
}

// deadlineFrom turns expires_in into a Unix second deadline.
func deadlineFrom(expiresIn int64, now time.Time) int64 {
	seconds := normalizeExpires(expiresIn)
	if seconds <= 0 {
		// The gateway always sends a lifetime; without one, assume the usual
		// twelve hours so the renewer still fires before the token dies.
		seconds = int64((12 * time.Hour) / time.Second)
	}
	return now.Add(time.Duration(seconds) * time.Second).Unix()
}

// RenewSession is the hourly maintenance hook. It is self-gated: an account
// whose token is still fresh, or which has no refresh token, costs nothing.
func (c *Client) RenewSession(ctx context.Context, accountID string) error {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		if strings.Contains(err.Error(), "凭证不完整") {
			return nil
		}
		return err
	}
	if !cred.CanRefresh() || !cred.NeedsRefresh(c.clock()) {
		return nil
	}
	_, err = c.refresh(ctx, accountID, cred)
	return err
}

// Probe proves the account can read its own catalog. A 200 on the models
// endpoint means the token is accepted; a 401 means it is dead, which is a
// credential problem rather than a busy upstream.
func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	cred, err := c.freshCredential(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{Ready: false, LastError: err.Error()}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	status, body, err := c.getModels(probeCtx, accountID, cred)
	if err != nil {
		return providers.AccountHealth{Ready: false, LastError: err.Error()}, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// One refresh attempt on a dead access token: the pair may simply be
		// old, and a refresh is cheaper for the operator than a new login.
		if renewed, refreshErr := c.refresh(ctx, accountID, cred); refreshErr == nil {
			if retryStatus, _, retryErr := c.getModels(probeCtx, accountID, renewed); retryErr == nil && retryStatus < 300 {
				return providers.AccountHealth{Ready: true, UID: renewed.Subject}, nil
			}
		}
		return providers.AccountHealth{Ready: false, LastError: fmt.Sprintf("minimax 鉴权失败 HTTP %d", status)}, nil
	}
	if status >= 300 {
		return providers.AccountHealth{Ready: false, LastError: fmt.Sprintf("minimax 目录 HTTP %d：%s", status, clip(string(body), 200))}, nil
	}
	return providers.AccountHealth{Ready: true, UID: cred.Subject}, nil
}

// Quota is unknown: the agent gateway bills a subscription the public API does
// not expose, and inventing a number would be worse than reporting none.
func (c *Client) Quota(context.Context, string) (*providers.QuotaInfo, error) {
	return nil, providers.ErrUnsupported
}

// Adapter wires the capability surface.
func (c *Client) Adapter() providers.Adapter {
	return providers.Adapter{
		ID:           ProviderID,
		Credential:   credentialCodec{},
		Chat:         c,
		Models:       c,
		Classifier:   classifier{},
		ImportExport: importer{},
		Prober:       c,
		Login:        c,
	}
}

type credentialCodec struct{}

func (credentialCodec) Validate(payload []byte) error { return ValidateCredential(payload) }

type classifier struct{}

func (classifier) Classify(status int, body string) providers.ClassifiedError {
	return Classify(status, body)
}

// dropCatalog clears the cached catalog so a rotated token does not keep serving
// a list fetched under the previous one.
func (c *Client) dropCatalog(accountID string) {
	c.mu.Lock()
	delete(c.catalogs, accountID)
	c.mu.Unlock()
}

// jsonEnvelopeIsError recognises the 2xx JSON body the gateway uses to report a
// refusal ({"error":…} or {"base_resp":{…}}) instead of a stream.
func jsonEnvelopeIsError(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return false
	}
	trimmed := strings.TrimSpace(string(body))
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	if _, ok := payload["error"]; ok {
		return true
	}
	if resp, ok := payload["base_resp"].(map[string]any); ok {
		if code, ok := resp["status_code"].(float64); ok && code != 0 {
			return true
		}
	}
	if _, ok := payload["type"].(string); ok {
		// An Anthropic error envelope carries type=error.
		kind, _ := payload["type"].(string)
		return kind == "error"
	}
	return false
}
