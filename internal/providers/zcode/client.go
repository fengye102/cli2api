package zcode

import (
	"context"
	"crypto/tls"
	"net/http"
	"runtime"
	"strings"
	"sync"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Version is the pinned ZCode client build the upstream still accepts. The
// console reports the same string via X-ZCode-App-Version.
const Version = "3.14.3"

// userAgent is the pinned ZCode desktop UA. The upstream WAF rejects unknown
// UAs on some endpoints, so keep this string verbatim.
func userAgent() string { return "ZCode/" + Version }

// defaultHeaders returns the header set every ZCode API call sends. The
// upstream logs these for routing decisions; do not add new ones.
func defaultHeaders() map[string]string {
	return map[string]string{
		"User-Agent":          userAgent(),
		"HTTP-Referer":        "https://zcode.z.ai",
		"X-Title":             "Z Code@electron",
		"X-ZCode-App-Version": Version,
		"X-Release-Channel":   "production",
		"X-Platform":          platformID(),
		"X-Client-Language":   "en-US",
		"X-Client-Timezone":   "UTC",
		// X-ZCode-Agent marks the calling product (glm = the ZCode agent
		// runtime the plan gateway serves). The desktop client sends it on
		// every plan-gateway call.
		"X-ZCode-Agent": "glm",
	}
}

func platformID() string {
	os := strings.ToLower(runtime.GOOS)
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	}
	return os + "-" + arch
}

// Store is the persistence surface the adapter needs. An in-process provider
// talks to the account store through this narrow interface and never imports
// internal/store. Mirrors command.Store.
type Store interface {
	Get(ctx context.Context, id string) (accounts.Account, error)
	LoadCredentialPayload(ctx context.Context, accountID string) (string, []byte, error)
	SaveCredentialPayload(ctx context.Context, accountID, format string, payload []byte) error
	Observe(ctx context.Context, id, remoteUID, status, lastError, lastKind string) error
}

// Client is the ZCode in-process adapter. It owns the credential codec, the
// live catalogue fetch, chat against the Anthropic Messages upstream, the
// browser-login rounds for both ZCode services (see login.go), and the liveness
// / quota probe backed by the plan-gateway balance endpoint.
type Client struct {
	store Store
	http  *http.Client

	// captcha mints the Aliyun traceless-verification tokens the plan
	// (OAuth) channel requires (see captcha.go).
	captcha *captchaPool

	// catalogURL overrides the catalogue endpoint; tests point this at a
	// local server.
	catalogURL string

	// tokenURL overrides the OAuth token endpoint; tests point this at a
	// local server.
	tokenURL string

	// businessLoginURL overrides the Z.ai business-login endpoint that mints
	// the ZCode JWT; tests point this at a local server.
	businessLoginURL string

	// userInfoURL overrides the realm userinfo endpoint used to fill the
	// account identity after login; tests point this at a local server.
	userInfoURL string

	// cliBaseURL overrides the plan-gateway base the Z.ai CLI sign-in runs on
	// (see login.go startCLILogin); tests point this at a local server.
	cliBaseURL string

	// mu guards pending, the in-flight browser-login rounds keyed by account
	// id (see login.go).
	mu      sync.Mutex
	pending map[string]*loginPending
}

// newUpstreamTransport builds the transport every ZCode request goes over. It
// speaks HTTP/1.1 only.
//
// The stock Go transport offers h2 in the TLS ALPN extension wherever the
// server suggests it, and zcode.z.ai does. Every real ZCode client speaks
// HTTP/1.1 instead: ZCode itself (Node's fetch), its OpenCode plugin (Bun's
// fetch) and zcode2api-plus (Go with h2 turned off). Alibaba Cloud's firewall
// can read that field, and it does: the same Start Plan request -- same path,
// same headers, same auth, same body -- was answered 405 / code 3012 "network
// firewall blocked" over h2 while a plugin run of the same account on the same
// machine at the same moment got through over h1. Nothing here needs h2 anyway:
// one turn is one stream, so there is no multiplexing to give up.
func newUpstreamTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return http1Only(base.Clone())
	}
	return http1Only(&http.Transport{Proxy: http.ProxyFromEnvironment})
}

// http1Only strips HTTP/2 from a transport: it neither offers h2 in ALPN nor
// accepts an h2 upgrade.
func http1Only(transport *http.Transport) *http.Transport {
	transport.ForceAttemptHTTP2 = false
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	// A non-nil empty map stops net/http from wiring HTTP/2 back in through the
	// TLSNextProto upgrade path.
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return transport
}

// NewClient builds the adapter. The http client has no global timeout so
// streaming responses can stay open; per-request timeouts are set by the
// caller.
func NewClient(store Store) *Client {
	client := &Client{
		store: store,
		http: &http.Client{
			Transport: newUpstreamTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	client.captcha = newCaptchaPool(client.http)
	return client
}

// SetBase satisfies the executor's optional base-override hook; ZCode
// upstreams come from the region descriptor and the credential's base_url,
// so the hook is a no-op here.
func (c *Client) SetBase(_ string) {}

// Adapter wires the capability surface: credential decode/validate, the
// browser login, the live+static catalogue, the
// import/export wizard, chat against the Anthropic Messages upstream, and a
// Prober backed by the plan-gateway balance endpoint.
func (c *Client) Adapter() providers.Adapter {
	return providers.Adapter{
		ID:           "zcode",
		Credential:   credentialCodec{},
		Login:        c,
		Models:       c,
		Chat:         c,
		Classifier:   classifier{},
		ImportExport: importer{},
		Prober:       c,
		Claim:        c,
	}
}

func (c *Client) String() string { return "zcode" }
