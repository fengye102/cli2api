package loomy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	proxyutil "github.com/caigee-cmd/cli2api/internal/proxy"
)

// Store is the persistence surface this adapter needs. It never imports
// internal/store, mirroring the other in-process providers.
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

type catalogCache struct {
	models []providers.ModelInfo
	at     time.Time
}

// Client is the in-process Loomy adapter. One client serves every account;
// credentials, device identities and catalogs stay keyed by account id.
type Client struct {
	store Store
	http  *http.Client

	transports proxyutil.TransportCache

	// capped caches the TLS-1.2 transport per proxy setting. The model plane
	// cannot complete a TLS 1.3 handshake (see doc.go), so its transport is
	// derived from the shared proxy transport and then pinned.
	cappedMu sync.Mutex
	capped   map[string]*http.Transport

	mu       sync.Mutex
	catalogs map[string]catalogCache

	// rounds holds the in-flight SMS login per account (login.go).
	roundsMu sync.Mutex
	rounds   map[string]*loginRound

	// Overrides for tests.
	modelBase   string
	accountBase string
}

func NewClient(store Store) *Client {
	return &Client{
		store: store,
		http: &http.Client{
			Timeout: 0,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		capped:      map[string]*http.Transport{},
		catalogs:    map[string]catalogCache{},
		rounds:      map[string]*loginRound{},
		modelBase:   modelBaseURL,
		accountBase: accountBaseURL,
	}
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

// httpClient returns a client for the account plane (regular TLS, honours the
// account proxy).
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

// modelClient returns a client for the model plane: same proxy, but pinned to
// TLS 1.2 and HTTP/1.1 because a TLS 1.3 handshake to the gateway never
// finishes.
func (c *Client) modelClient(ctx context.Context, accountID string) (*http.Client, error) {
	rawProxy, err := c.effectiveProxy(ctx, accountID)
	if err != nil {
		return nil, err
	}
	transport, err := c.cappedTransport(rawProxy)
	if err != nil {
		return nil, err
	}
	client := *c.http
	client.Transport = transport
	return &client, nil
}

// cappedTransport builds (once per proxy setting) a transport identical to the
// shared one except that TLS is capped at 1.2 and HTTP/2 is off.
func (c *Client) cappedTransport(rawProxy string) (*http.Transport, error) {
	c.cappedMu.Lock()
	defer c.cappedMu.Unlock()
	if transport, ok := c.capped[rawProxy]; ok {
		return transport, nil
	}

	shared, err := c.transports.Get(rawProxy)
	if err != nil {
		return nil, err
	}
	var transport *http.Transport
	if shared != nil {
		transport = shared.Clone()
	} else {
		transport = defaultTransport()
	}

	tlsConfig := &tls.Config{MaxVersion: tls.VersionTLS12}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
		tlsConfig.MaxVersion = tls.VersionTLS12
		// A MinVersion above 1.2 would make every request fail instead of
		// falling back, so it is cleared.
		tlsConfig.MinVersion = 0
	}
	transport.TLSClientConfig = tlsConfig
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}

	c.capped[rawProxy] = transport
	return transport, nil
}

// defaultTransport mirrors http.DefaultTransport's dial behaviour without the
// shared instance (whose TLS config is process-wide).
func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 0,
	}
}

func (c *Client) credentials(ctx context.Context, accountID string) (Credential, error) {
	_, payload, err := c.store.LoadCredentialPayload(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	return DecodeCredential(payload)
}

// credential loads the stored credential and guarantees a usable session,
// logging in with the stored password when the session is missing or about to
// expire.
func (c *Client) credential(ctx context.Context, accountID string) (Credential, error) {
	cred, err := c.credentials(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	session := strings.TrimSpace(cred.Session)
	fresh := session != "" && (cred.ExpiresAt == 0 || time.Now().Unix() < cred.ExpiresAt-int64(sessionRenewBefore/time.Second))
	if fresh {
		return cred, nil
	}
	if !cred.CanRenew() {
		if session != "" {
			// No expiry metadata and no password: still worth a try, the
			// gateway is authoritative.
			return cred, nil
		}
		return cred, fmt.Errorf("loomy credential has no session and no password to log in with")
	}
	return c.loginAndStore(ctx, accountID, cred)
}

// relogin forces a fresh session, used after upstream answers 401.
func (c *Client) relogin(ctx context.Context, accountID string) (Credential, error) {
	cred, err := c.credentials(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	if !cred.CanRenew() {
		return cred, fmt.Errorf("loomy session expired and the credential carries no password to renew it")
	}
	return c.loginAndStore(ctx, accountID, cred)
}

func (c *Client) save(ctx context.Context, accountID string, cred Credential) error {
	payload, err := EncodeCredential(cred)
	if err != nil {
		return err
	}
	if err := c.store.SaveCredentialPayload(ctx, accountID, CredentialFormat, payload); err != nil {
		return err
	}
	return c.store.Observe(ctx, accountID, cred.UserID, "ready", "", "")
}

// Probe refreshes readiness by reading the model catalog. A quota failure is
// not a readiness failure: it is reported through Quota.
func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	if _, err := c.fetchCatalog(ctx, accountID, cred); err != nil {
		if providerErr, ok := err.(*providers.Error); ok {
			_ = c.store.Observe(ctx, accountID, cred.UserID, "", providerErr.Message, providerErr.Kind)
			return providers.AccountHealth{LastError: providerErr.Message}, nil
		}
		_ = c.store.Observe(ctx, accountID, cred.UserID, "", err.Error(), "")
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	_ = c.store.Observe(ctx, accountID, cred.UserID, "ready", "", "")
	return providers.AccountHealth{Ready: true, Hot: true, UID: cred.UserID}, nil
}

// Adapter wires the capability surface.
func (c *Client) Adapter() providers.Adapter {
	return providers.Adapter{
		ID:           "loomy",
		Credential:   credentialCodec{},
		Chat:         c,
		Models:       c,
		Classifier:   classifier{},
		Login:        loginFlow{client: c},
		ImportExport: importer{},
		Prober:       c,
		Claim:        c,
		ImageGen:     c,
	}
}

type credentialCodec struct{}

func (credentialCodec) Validate(payload []byte) error { return ValidateCredential(payload) }

type classifier struct{}

func (classifier) Classify(status int, body string) providers.ClassifiedError {
	return Classify(status, body)
}
