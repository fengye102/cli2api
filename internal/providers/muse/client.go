package muse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	proxyutil "github.com/caigee-cmd/cli2api/internal/proxy"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// Store is the persistence surface this adapter needs.
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

// Client is the in-process muse.ai adapter. It owns credentials, proxying and
// health; the conversation itself goes through a ChatTransport so that the
// HTTP implementation and the (fallback) browser implementation stay
// swappable without touching the account pool.
type Client struct {
	store Store
	http  *http.Client

	transports proxyutil.TransportCache
	chat       ChatTransport

	nowFn func() time.Time

	// sessionURL is a test override for SessionAPI.
	sessionURL string

	mu sync.Mutex
}

// NewClient builds the adapter. The chat transport defaults to the pure-HTTP
// implementation; tests may replace it via SetChatTransport.
func NewClient(store Store) *Client {
	return &Client{
		store: store,
		http: &http.Client{
			Timeout: 0,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		chat:  NewHTTPTransport(),
		nowFn: time.Now,
	}
}

// SetChatTransport swaps the conversation transport (used by tests and by the
// console when the browser fallback has to take over).
func (c *Client) SetChatTransport(transport ChatTransport) {
	c.mu.Lock()
	c.chat = transport
	c.mu.Unlock()
}

func (c *Client) chatTransport() ChatTransport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chat
}

func (c *Client) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

func (c *Client) globalProxy(ctx context.Context) (string, error) {
	store, ok := c.store.(SecretReader)
	if !ok {
		return "", nil
	}
	value, found, err := store.GetSecret(ctx, "proxy_url")
	if err != nil || !found {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (c *Client) httpClient(ctx context.Context, accountID string) (*http.Client, error) {
	proxyURL, err := c.globalProxy(ctx)
	if err != nil {
		return nil, err
	}
	if accountID != "" {
		account, accErr := c.store.Get(ctx, accountID)
		switch {
		case accErr == nil:
			if p := strings.TrimSpace(account.ProxyURL); p != "" {
				proxyURL = p
			}
		case errors.Is(accErr, accounts.ErrAccountNotFound):
			// Import can run before the row exists; nothing to prefer over the
			// global proxy.
		default:
			return nil, fmt.Errorf("muse: resolve proxy for account %s: %w", accountID, accErr)
		}
	}
	transport, err := c.transports.Get(proxyURL)
	if err != nil {
		return nil, err
	}
	client := *c.http
	if transport != nil {
		client.Transport = transport
	}
	client.Timeout = 0
	return &client, nil
}

func (c *Client) loadCredential(ctx context.Context, accountID string) (Credential, error) {
	_, payload, err := c.store.LoadCredentialPayload(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	return DecodeCredential(payload)
}

func (c *Client) saveCredential(ctx context.Context, accountID string, credential Credential) error {
	payload, err := credential.Encode()
	if err != nil {
		return err
	}
	format := credential.Format
	if format == "" {
		format = CredentialFormat
	}
	return c.store.SaveCredentialPayload(ctx, accountID, format, payload)
}

func (c *Client) credential(ctx context.Context, accountID string) (Credential, error) {
	credential, err := c.loadCredential(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	if !credential.Ready() {
		// Name the missing cookie: "incomplete or expired" alone sends the
		// operator hunting through an export that may simply be the wrong tab.
		message := "muse session storage is not complete; re-import cookies from a logged-in muse.ai tab"
		if err := RequireSessionCookies(credential); err != nil {
			message = err.Error()
		}
		return Credential{}, &providers.Error{
			Kind:    accounts.KindAuth,
			Status:  http.StatusUnauthorized,
			Message: message,
		}
	}
	return credential, nil
}

// sessionSnapshot is the subset of /api/session this adapter reads. muse.ai's
// payload is not contractual, so every field is optional and parsing never
// fails the probe.
type sessionSnapshot struct {
	Email string
	UID   string
	// Kind is "loggedOut" for an anonymous session.
	Kind string
	// VMState is the agent VM state when the payload carries one.
	VMState string
}

// Probe checks the session with GET /api/session. A 200 means the cookie jar
// still holds a live session, which is the only readiness signal muse.ai
// offers.
func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		health := providers.AccountHealth{LastError: providerMessage(err)}
		return health, nil
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()
	sessionURL := c.sessionURL
	if sessionURL == "" {
		sessionURL = SessionAPI
	}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, sessionURL, nil)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	applyWebHeaders(req, credential)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return providers.AccountHealth{LastError: fmt.Sprintf("muse session probe failed: %v", err)}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		health := providers.AccountHealth{LastError: "muse session expired (HTTP " + fmt.Sprint(resp.StatusCode) + "); re-import cookies from a logged-in tab"}
		_ = c.store.Observe(ctx, accountID, "", "offline", health.LastError, accounts.KindAuth)
		return health, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		health := providers.AccountHealth{LastError: fmt.Sprintf("muse session probe returned HTTP %d", resp.StatusCode)}
		_ = c.store.Observe(ctx, accountID, "", "offline", health.LastError, accounts.KindUnavailable)
		return health, nil
	}

	snapshot := parseSessionSnapshot(body)
	if snapshot.Kind == "loggedOut" {
		health := providers.AccountHealth{LastError: "muse session is logged out; re-import cookies from a logged-in tab"}
		_ = c.store.Observe(ctx, accountID, "", "offline", health.LastError, accounts.KindAuth)
		return health, nil
	}

	uid := firstNonEmpty(snapshot.Email, snapshot.UID)
	if uid != "" && (credential.Email != snapshot.Email || credential.UID != snapshot.UID) {
		credential.Email = firstNonEmpty(snapshot.Email, credential.Email)
		credential.UID = firstNonEmpty(snapshot.UID, credential.UID)
		if err := c.saveCredential(ctx, accountID, credential); err != nil {
			// Labelling is best effort; a write failure must not fail a probe.
			_ = err
		}
	}
	_ = c.store.Observe(ctx, accountID, uid, "ready", "", "")
	return providers.AccountHealth{Ready: true, Hot: true, UID: uid}, nil
}

// Quota returns nil: muse.ai exposes usage in the web UI (Settings → Usage)
// only through a browser driver, so the console shows "unknown" instead of a
// fabricated zero.
func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	_ = ctx
	_ = accountID
	return nil, nil
}

// Models returns the static muse catalog.
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	_ = ctx
	_ = accountID
	return catalogModelInfos(), nil
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
	}
}

type credentialCodec struct{}

func (credentialCodec) Validate(payload []byte) error { return ValidateCredential(payload) }

type classifier struct{}

func (classifier) Classify(status int, body string) providers.ClassifiedError {
	return Classify(status, body)
}

// ChatNonStream runs one completion through the transport and collects it.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	transport := c.chatTransport()
	if transport == nil || !transport.Available() {
		return providers.ChatOutcome{}, errTransportPending()
	}
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	return transport.Chat(ctx, chatInput{AccountID: accountID, Credential: credential, Request: req})
}

// ChatStream runs one streaming completion. The transport returns an
// OpenAI-delta SSE body so the API layer relays it unchanged.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	transport := c.chatTransport()
	if transport == nil || !transport.Available() {
		return nil, providers.ResolvedChat{}, errTransportPending()
	}
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	return transport.ChatStream(ctx, chatInput{AccountID: accountID, Credential: credential, Request: req})
}

func providerMessage(err error) string {
	var providerErr *providers.Error
	if errors.As(err, &providerErr) && providerErr.Message != "" {
		return providerErr.Message
	}
	return err.Error()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// applyWebHeaders stamps the headers a muse.ai browser request carries.
func applyWebHeaders(req *http.Request, credential Credential) {
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Origin", SiteURL)
	req.Header.Set("Referer", NewThreadURL)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cookie", credential.CookieHeader())
}
