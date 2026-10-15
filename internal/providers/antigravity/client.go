package antigravity

import (
	"context"
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
// internal/store.
type Store interface {
	Get(ctx context.Context, id string) (accounts.Account, error)
	LoadCredentialPayload(ctx context.Context, accountID string) (string, []byte, error)
	SaveCredentialPayload(ctx context.Context, accountID, format string, payload []byte) error
	Observe(ctx context.Context, id, remoteUID, status, lastError, lastKind string) error
}

// regionEgressSecret is the app secret naming a proxy to use when Google
// rejects the gateway's own egress region. It is separate from `proxy_url` so
// the region workaround can be scoped to Antigravity without rerouting every
// other provider.
const regionEgressSecret = "antigravity_region_egress"

// SecretReader is optional. Missing it means no global proxy.
type SecretReader interface {
	GetSecret(context.Context, string) (string, bool, error)
}

type catalogCache struct {
	models []providers.ModelInfo
	at     time.Time
}

// Client is the in-process Antigravity adapter. One client serves every
// account; credentials, catalogs and login rounds stay keyed by account id.
type Client struct {
	store Store
	http  *http.Client

	transports proxyutil.TransportCache

	mu       sync.Mutex
	catalogs map[string]catalogCache
	rounds   map[string]*loginRound
	listener net.Listener

	// refreshing dedupes concurrent token renewals for one account, so N
	// in-flight requests do not each burn a refresh round.
	refreshing map[string]*refreshFlight
}

// refreshFlight is one in-flight token renewal other callers can wait on.
type refreshFlight struct {
	done chan struct{}
	cred Credential
	err  error
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
		catalogs:   map[string]catalogCache{},
		rounds:     map[string]*loginRound{},
		refreshing: map[string]*refreshFlight{},
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

func (c *Client) httpClient(ctx context.Context, accountID string) (*http.Client, error) {
	rawProxy, err := c.effectiveProxy(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return c.httpClientWithProxy(rawProxy)
}

// httpClientWithProxy builds a client for an explicit proxy setting ("" keeps
// the direct transport). Used both by the normal path and by the region-egress
// retry, so both share one transport cache.
func (c *Client) httpClientWithProxy(rawProxy string) (*http.Client, error) {
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

// regionEgress returns the proxy to retry through when Google rejects the
// default egress region. It reads the dedicated `antigravity_region_egress`
// secret first, so an operator can route Antigravity through a supported region
// without disturbing every other provider's proxy setting; the global
// `proxy_url` is the fallback.
//
// An empty return means "no alternative egress configured" — the caller then
// surfaces the explicit region-restriction error instead of retrying.
func (c *Client) regionEgress(ctx context.Context) string {
	store, ok := c.store.(SecretReader)
	if !ok {
		return ""
	}
	if value, found, err := store.GetSecret(ctx, regionEgressSecret); err == nil && found {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	value, found, err := store.GetSecret(ctx, "proxy_url")
	if err != nil || !found {
		return ""
	}
	return strings.TrimSpace(value)
}

func (c *Client) credential(ctx context.Context, accountID string) (Credential, error) {
	_, payload, err := c.store.LoadCredentialPayload(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	cred, err := DecodeCredential(payload)
	if err != nil {
		return Credential{}, err
	}
	if !cred.Ready() {
		return cred, fmt.Errorf("antigravity credential incomplete; paste a Gemini API key")
	}
	return cred, nil
}

// Probe checks that the key can read its own model list. A quota failure is
// not a readiness failure; quota is probed separately (and unknown here).
func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	if _, err := c.fetchCatalog(ctx, accountID, credential); err != nil {
		if providerErr, ok := err.(*providers.Error); ok {
			return providers.AccountHealth{LastError: providerErr.Message}, nil
		}
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	_ = c.store.Observe(ctx, accountID, "", "ready", "", "")
	return providers.AccountHealth{Ready: true, Hot: true}, nil
}

// Adapter wires the capability surface. Login is present: besides the pasted
// Gemini API key, the account can be created through Google browser login.
func (c *Client) Adapter() providers.Adapter {
	return providers.Adapter{
		ID:           "antigravity",
		Credential:   credentialCodec{},
		Login:        c,
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
