package zcode

import (
	"context"
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

	// customerURL overrides the BigModel customer-info fallback endpoint (see
	// login.go realmIdentity); tests point this at a local server.
	customerURL string

	// cliBaseURL overrides the plan-gateway base the Z.ai CLI sign-in runs on
	// (see login.go startCLILogin); tests point this at a local server.
	cliBaseURL string

	// mu guards pending, the in-flight browser-login rounds keyed by account
	// id (see login.go).
	mu      sync.Mutex
	pending map[string]*loginPending
}

// NewClient builds the adapter. The http client has no global timeout so
// streaming responses can stay open; per-request timeouts are set by the
// caller.
func NewClient(store Store) *Client {
	client := &Client{
		store: store,
		http: &http.Client{
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
// browser login for both ZCode services, the live+static catalogue, the
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
	}
}

func (c *Client) String() string { return "zcode" }
