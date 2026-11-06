package minimax

import "time"

const (
	// ProviderID is the registry id and the model prefix that pins a route to
	// this provider.
	ProviderID = "minimax"

	// Label is what the console shows for the channel.
	Label = "MiniMax Code"

	// CredentialFormat is the canonical stored credential format.
	CredentialFormat = "minimax-oauth-v1"

	// PublicClientID is the CLI's own OAuth client. It is public by design:
	// the device flow authenticates the user, not the client.
	PublicClientID = "mcode-public"
	Scope          = "agent.default"
	Audience       = "agent-backend"

	// DeviceGrant is the RFC 8628 grant type the token endpoint expects.
	DeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

	// AgentID is the mavis agent the gateway serves. The official client sends
	// it on every request; a request without it is treated as an unknown agent.
	AgentID = "main"

	// UserAgent matches the official client. The agent gateway fronts the same
	// WAF as the web console, so a Go default UA is the first thing to get
	// rejected.
	UserAgent = "MiniMaxAgent"

	// AnthropicVersion is the Messages protocol version the gateway speaks.
	AnthropicVersion = "2023-06-01"

	// PlaceholderAPIKey is sent next to the Bearer token because the gateway
	// requires the field to exist, exactly as the official client does.
	PlaceholderAPIKey = "sk-xxx"

	// MessagesPath is the Anthropic Messages endpoint, relative to the agent
	// host of the account's region.
	MessagesPath = "/mavis/api/v1/llm/v1/messages"

	// ModelsPath is the live catalog endpoint. The region query is required:
	// the gateway resolves pricing and entitlements from it.
	ModelsPath = "/mavis/api/v1/models?region=%s&buildEnv=prod"

	// DeviceCodePath and TokenPath live on the account host of the region.
	DeviceCodePath = "/oauth2/device/code"
	TokenPath      = "/oauth2/token"

	// CatalogTTL bounds how stale the model list may get. The gateway itself
	// declares ttlSeconds=300, so refreshing at the same cadence is enough.
	CatalogTTL = 5 * time.Minute

	// ProbeTimeout bounds the readiness check so a dead gateway cannot hang the
	// console's test button.
	ProbeTimeout = 25 * time.Second

	// refreshLead is how long before expiry the adapter renews a token on its
	// own. Access tokens live ~12h; the hourly renewer therefore fires with
	// hours of headroom.
	refreshLead = 45 * time.Minute

	// loginStateTTL is how long a started device-code round stays pollable.
	loginStateTTL = 15 * time.Minute

	// defaultMaxTokens is used when neither the client nor the catalog declare
	// an output limit. The Messages API requires a positive value.
	defaultMaxTokens = 8192
)

// Site is one MiniMax Code deployment. The two sites share the wire shape and
// differ only in host and region code.
type Site struct {
	// Region is the registry region id and the `region` query value.
	Region string
	// Label is the console label.
	Label string
	// AccountHost serves the OAuth endpoints.
	AccountHost string
	// AgentHost serves messages and the catalog.
	AgentHost string
}

var (
	siteCN = Site{
		Region:      "cn",
		Label:       "国内版",
		AccountHost: "https://account.minimax.cn",
		AgentHost:   "https://agent.minimax.cn",
	}
	siteGlobal = Site{
		Region:      "global",
		Label:       "国际版",
		AccountHost: "https://account.minimax.io",
		AgentHost:   "https://agent.minimax.io",
	}
)

// siteFor resolves a stored region onto a site. An unknown or empty region
// falls back to the mainland deployment, which is the default in the console.
func siteFor(region string) Site {
	if region == siteGlobal.Region {
		return siteGlobal
	}
	return siteCN
}

// staticCatalog is the fallback model list. MiniMax ships models faster than the
// live catalog is guaranteed to answer, and an account whose catalog call fails
// must still be usable: a model absent here is added from the live catalog on
// the next successful refresh.
type staticModel struct {
	ID           string
	Name         string
	Context      int
	MaxOutput    int
	Tools        bool
	Images       bool
	Reasoning    bool
	PreviewStage string
}

var staticCatalog = []staticModel{
	{ID: "MiniMax-M3", Name: "MiniMax M3", Context: 512_000, MaxOutput: 128_000, Tools: true, Images: true, Reasoning: true},
	{ID: "MiniMax-M3.1-Flash", Name: "MiniMax M3.1 Flash", Context: 512_000, MaxOutput: 128_000, Tools: true, Images: true, Reasoning: true},
	{ID: "MiniMax-M3.1-Flash-Preview", Name: "MiniMax M3.1 Flash Preview", Context: 512_000, MaxOutput: 128_000, Tools: true, Images: true, Reasoning: true, PreviewStage: "preview"},
	{ID: "MiniMax-M2.7", Name: "MiniMax M2.7", Context: 204_800, MaxOutput: 32_768, Tools: true, Images: false, Reasoning: false},
	{ID: "MiniMax-M2.7-highspeed", Name: "MiniMax M2.7 Highspeed", Context: 204_800, MaxOutput: 32_768, Tools: true, Images: false, Reasoning: false},
}
