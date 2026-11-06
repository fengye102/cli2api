package custom

import (
	"context"
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

// SecretReader is optional. Missing it means no global proxy.
type SecretReader interface {
	GetSecret(context.Context, string) (string, bool, error)
}

type catalogCache struct {
	models []providers.ModelInfo
	at     time.Time
}

// Client is the in-process custom-relay adapter. One client serves every
// account; each account carries its own base URL, so several relays can be
// registered side by side and the pool routes by model.
type Client struct {
	store Store
	http  *http.Client

	transports proxyutil.TransportCache

	mu       sync.Mutex
	catalogs map[string]catalogCache
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
		catalogs: map[string]catalogCache{},
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

func (c *Client) credential(ctx context.Context, accountID string) (Credential, error) {
	_, payload, err := c.store.LoadCredentialPayload(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	credential, err := DecodeCredential(payload)
	if err != nil {
		return Credential{}, err
	}
	if !credential.Ready() {
		return credential, fmt.Errorf("custom 账号凭证不完整：需要 base_url 与 api_key")
	}
	return credential, nil
}

// Probe proves the key can read its own model list. A relay that answers 200 on
// /models is usable; anything else is a broken account, not a busy one, so it is
// reported through LastError and left out of rotation.
func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	models, err := c.fetchCatalog(probeCtx, accountID, credential)
	if err != nil {
		c.dropCatalog(accountID)
		if providerErr, ok := err.(*providers.Error); ok {
			return providers.AccountHealth{LastError: providerErr.Message}, nil
		}
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	c.storeCatalog(accountID, models)
	_ = c.store.Observe(ctx, accountID, "", "ready", "", "")
	return providers.AccountHealth{Ready: true, Hot: true, UID: credential.Label()}, nil
}

// Quota is unknown by design: relays rarely expose a balance endpoint this
// adapter could read generically, and inventing a number would be worse than
// reporting none.
func (c *Client) Quota(context.Context, string) (*providers.QuotaInfo, error) {
	return nil, providers.ErrUnsupported
}

// Adapter wires the capability surface. Login is absent on purpose: the only
// auth is a pasted base URL plus key.
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

// dropCatalog clears the cached catalog after a failed probe, so a fixed
// key does not serve a stale list.
func (c *Client) dropCatalog(accountID string) {
	c.mu.Lock()
	delete(c.catalogs, accountID)
	c.mu.Unlock()
}

type credentialCodec struct{}

func (credentialCodec) Validate(payload []byte) error { return ValidateCredential(payload) }

type classifier struct{}

func (classifier) Classify(status int, body string) providers.ClassifiedError {
	return Classify(status, body)
}
