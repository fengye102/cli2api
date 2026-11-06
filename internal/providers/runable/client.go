package runable

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

// Store is the persistence surface this adapter needs.
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

// Client is the in-process Runable adapter. One client serves every account;
// each account carries its own web cookie.
type Client struct {
	store Store
	http  *http.Client

	transports proxyutil.TransportCache
	// baseURL lets a test point the client at a fake edge. Empty means BaseURL.
	baseURL string

	mu       sync.Mutex
	catalogs map[string]catalogCache
}

func NewClient(store Store) *Client {
	return &Client{
		store: store,
		http: &http.Client{
			// A turn streams for minutes; deadlines are per-request.
			Timeout: 0,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		catalogs: map[string]catalogCache{},
	}
}

func (c *Client) origin() string {
	if base := strings.TrimRight(strings.TrimSpace(c.baseURL), "/"); base != "" {
		return base
	}
	return BaseURL
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
	cred, err := DecodeCredential(payload)
	if err != nil {
		return Credential{}, err
	}
	if !cred.Ready() {
		return cred, fmt.Errorf("runable 账号凭证不完整：需要网页 cookie")
	}
	return cred, nil
}

// applyHeaders sets what the web client sends. The cookie is the credential; the
// device header is a constant that identifies the client build.
func applyHeaders(h http.Header, cred Credential) {
	h.Set("Cookie", cred.Cookie)
	h.Set("x-runable-device", DeviceHeader)
	h.Set("User-Agent", UserAgent)
	h.Set("Origin", "https://runable.com")
	h.Set("Referer", "https://runable.com/")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
}

// Probe reads the model catalog: a cookie the edge still accepts answers 200
// with a model list, a dead one answers 401.
func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{Ready: false, LastError: err.Error()}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	status, body, err := c.fetchModels(probeCtx, accountID, cred)
	if err != nil {
		return providers.AccountHealth{Ready: false, LastError: err.Error()}, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return providers.AccountHealth{Ready: false,
			LastError: "runable 网页登录态已失效，请重新粘贴 cookie"}, nil
	}
	if status >= 300 {
		classified := Classify(status, string(body))
		return providers.AccountHealth{Ready: false, LastError: classified.Message}, nil
	}
	models, parseErr := parseCatalog(body)
	if parseErr != nil {
		// The session is valid; only the catalog shape confused us. Readiness
		// must not depend on a parse detail.
		return providers.AccountHealth{Ready: true, UID: cred.UserID}, nil
	}
	if len(models) == 0 {
		return providers.AccountHealth{Ready: false, LastError: "runable 账号没有可用模型"}, nil
	}
	return providers.AccountHealth{Ready: true, UID: firstNonEmpty(cred.UserID, cred.Email)}, nil
}

// Quota is unknown: the account view is a web session, not a billing API this
// adapter can read. Runable reports an exhausted free quota as a 402 on chat,
// which the classifier turns into a quota cooldown.
func (c *Client) Quota(context.Context, string) (*providers.QuotaInfo, error) {
	return nil, providers.ErrUnsupported
}

// Adapter wires the capability surface. Login is absent on purpose: the only
// auth is a cookie copied out of a signed-in browser, which this process cannot
// capture by itself.
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

func (c *Client) dropCatalog(accountID string) {
	c.mu.Lock()
	delete(c.catalogs, accountID)
	c.mu.Unlock()
}
