// Package autoclaw implements the AutoClaw (智谱 autoglm / OpenClaw) in-process
// provider.
//
// AutoClaw is a desktop AI client whose local proxy forwards to an upstream
// "autoclaw-proxy" gateway. Two builds exist: a domestic one (CN, the
// autoglm-acceleration-api.zhipuai.cn site) and an overseas one (Intl,
// autoglm-api.autoglm.ai). They share the protocol and the client fingerprint
// (X-Auth-Sign appId/appKey are byte-identical on both) but use different
// hosts, so each build is its own provider family: autoclaw (CN) and
// autoclaw-intl (Intl). Same reasoning as raccoon/Cline: the region is a
// channel, not an account attribute.
//
// The upstream gateway pre-validates request shape and rejects non-conforming
// requests before they reach the model. Three gates are implemented here
// because each one turns a perfectly ordinary client request into an empty
// error body:
//
//   - system prompt: must begin with the OpenClaw identity line and carry a
//     "## Tooling" section, and must not contain a foreign harness identity
//     ("You are Codex", "You are Claude Code", ...). Missing or foreign ->
//     406 with an empty body (or 403 pay-view).
//   - max_tokens: small positive values (< 10000) are rejected outright with
//     406; the gateway normalizes them up to 10000.
//   - request headers: X-Harness-Type: zcode yields 403 pay-view / 406 on the
//     chat path, so it is deliberately NOT sent there (it IS sent on the
//     userapi path, where it is part of the official fingerprint).
//
// Credentials are access tokens (JWT) plus a refresh token. Refresh is a
// signed POST to the userapi host; the signature is MD5(appId&ts&appKey) with
// a second-resolution timestamp.
package autoclaw

// CredentialFormat is the canonical storage payload name for this provider.
const CredentialFormat = "autoclaw-credential-v1"

// IntlCredentialFormat is the storage payload name for the overseas build.
const IntlCredentialFormat = "autoclaw-intl-credential-v1"

// ProviderID / IntlProviderID are the provider ids registered in the catalog.
const (
	ProviderID     = "autoclaw"
	IntlProviderID = "autoclaw-intl"
)

// Client fingerprint. These are the desktop client's embedded values, not our
// secrets: X-Auth-Sign is MD5("<appID>&<seconds>&<appKey>") and the upstream
// recognizes the official client by it. Hardcoding is correct here - making
// them configurable would only let the signature drift out of sync.
const (
	authAppID  = "100003"
	authAppKey = "38d2391985e2369a5fb8227d8e6cd5e5"
)

// Client identity headers.
//
// desktopAppVersion is sent as X-Version on the chat path. The userapi path
// uses userapiVersion instead: the upstream gates the model catalog on
// X-Version, and an older value silently downgrades the advertised model list
// rather than erroring.
const (
	desktopAppVersion = "1.17.8"
	userapiVersion    = "1.18.5"
	clientLang        = "zh-CN"
	clientChannel     = "official"
	clientProduct     = "autoclaw"
	clientType        = "pc"
	clientTraceID     = "autoclaw-desktop"
	harnessType       = "zcode"
)

// Endpoints. upstreamBase already carries the trailing "/autoclaw".
const (
	cnUpstreamBase   = "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw"
	intlUpstreamBase = "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw"

	cnUserapiBase   = "https://autoglm-acceleration-api.zhipuai.cn"
	intlUserapiBase = "https://autoglm-api.autoglm.ai"

	// chatPath is appended to the upstream base. The CN build has no /v1
	// segment; the Intl build does. Both are spelled out because getting this
	// wrong yields a 404 that looks like an auth failure.
	cnChatPath   = "/chat/completions"
	intlChatPath = "/v1/chat/completions"

	// refreshPath mints a new access token. refreshFallbackPath is the
	// degraded variant used when the signed request is rejected with code
	// 400002.
	refreshPath         = "/userapi/v1/refresh"
	refreshFallbackPath = "/userapi/v1/agent-refresh"

	// catalogPath serves the remote model catalog. Note it lives one level up
	// from the chat base (/proxy/, not /proxy/autoclaw) - walking down from
	// chat/completions will never find it.
	catalogPath = "/autoclaw-proxy/proxy/autoclaw-model-config"

	// balancePath and subscribePath back the quota display.
	balancePath   = "/userapi/v1/wallets/v2"
	subscribePath = "/userapi/v1/subscribe-info"
)

// Timeouts, in seconds. metadataTimeout keeps catalog/balance probes short;
// probeTimeout keeps liveness checks short. dialTimeout bounds a blocked
// route so it fails fast instead of hanging on kernel TCP retries.
const (
	dialTimeoutSeconds     = 10
	metadataTimeoutSeconds = 30
	probeTimeoutSeconds    = 10
	catalogCacheTTL        = 10 * 60
)

// maxTokensFloor is the smallest max_tokens the upstream accepts. Values
// between 1 and 9999 are rewritten to this; absent values are left absent.
const maxTokensFloor = 10000

// refreshExpiringWindow is how close to expiry a token must be before a
// request-triggered refresh. The upstream rotates refresh tokens, and a
// long-idle one can be invalidated, so maintenance also refreshes hourly.
const (
	refreshExpiringWindow = 5 * 60
	hourlyForcedRefresh   = 60 * 60
)

// defaultRoute is the fallback route when the client names an unknown model.
const defaultRoute = "zai_auto"
