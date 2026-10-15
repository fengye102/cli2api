package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// SecretReader is optional. Missing it means no global proxy.
type SecretReader interface {
	GetSecret(context.Context, string) (string, bool, error)
}

type catalogCache struct {
	models []providers.ModelInfo
	at     time.Time
}

// Client is the in-process Raccoon adapter. One client serves every account;
// credentials and catalogs stay keyed by account id.
type Client struct {
	store Store
	http  *http.Client

	transports proxyutil.TransportCache

	mu       sync.Mutex
	catalogs map[string]catalogCache

	// llmBase / authBase / siteBase overrides for tests.
	llmBase  string
	authBase string
	siteBase string

	// rounds holds the in-flight browser-login round per account (login.go).
	rounds map[string]*loginRound
	// listener is the loopback callback listener, bound once and kept for the
	// process lifetime so the redirect_uri stays stable (login.go).
	listener net.Listener
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
		rounds:   map[string]*loginRound{},
		llmBase:  llmBaseURL,
		authBase: authBaseURL,
		siteBase: siteBaseURL,
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
	cred, err := DecodeCredential(payload)
	if err != nil {
		return Credential{}, err
	}
	if !cred.Ready() {
		return cred, fmt.Errorf("raccoon credential incomplete; import a token or refresh token")
	}
	return cred, nil
}

// refresh mints a new access token from the refresh token. Returns the updated
// credential (with the new access token and any rotated refresh token).
func (c *Client) refresh(ctx context.Context, accountID string, cred Credential) (Credential, error) {
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return cred, fmt.Errorf("raccoon refresh needs a refresh token")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": cred.RefreshToken})
	endpoint := strings.TrimRight(c.authBase, "/") + refreshPath
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return cred, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return cred, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return cred, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return cred, fmt.Errorf("raccoon refresh failed (status=%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return cred, fmt.Errorf("raccoon refresh: malformed response: %w", err)
	}
	updated := cred
	if out.AccessToken != "" {
		updated.AccessToken = strings.TrimSpace(out.AccessToken)
	}
	if out.RefreshToken != "" {
		updated.RefreshToken = strings.TrimSpace(out.RefreshToken)
	}
	// Persist the rotated credential so the new access token survives restarts.
	if payload, err := EncodeCredential(updated); err == nil {
		_ = c.store.SaveCredentialPayload(ctx, accountID, "raccoon-credential-v1", payload)
	}
	return updated, nil
}

// Probe checks that the credential can reach the model catalog. A quota
// failure is not a readiness failure.
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
	_ = c.store.Observe(ctx, accountID, credential.UserID, "ready", "", "")
	return providers.AccountHealth{Ready: true, Hot: true, UID: credential.UserID}, nil
}

// Adapter wires the capability surface. Login is the browser round
// (login.go): Raccoon's own web client signs in through a custom-scheme
// callback, so the console finishes it from a pasted callback URL.
func (c *Client) Adapter() providers.Adapter {
	return providers.Adapter{
		ID:           "raccoon",
		Credential:   credentialCodec{},
		Chat:         c,
		Models:       c,
		Classifier:   classifier{},
		Login:        c,
		ImportExport: importer{},
		Prober:       c,
		Checkin:      c,
	}
}

type credentialCodec struct{}

func (credentialCodec) Validate(payload []byte) error { return ValidateCredential(payload) }

type classifier struct{}

func (classifier) Classify(status int, body string) providers.ClassifiedError {
	return Classify(status, body)
}
