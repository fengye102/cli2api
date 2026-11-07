package providers

import (
	"fmt"
	"strings"
)

type RuntimeKind string

const (
	RuntimeChildProcess RuntimeKind = "child_process"
	RuntimeInProcess    RuntimeKind = "in_process"
)

type AuthType string

const (
	AuthNone   AuthType = "none"
	AuthOAuth  AuthType = "oauth"
	AuthPAT    AuthType = "pat"
	AuthNative AuthType = "native"
)

type ProviderCapabilities struct {
	Chat         bool `json:"chat"`
	Stream       bool `json:"stream"`
	Tools        bool `json:"tools"`
	Images       bool `json:"images"`
	Reasoning    bool `json:"reasoning"`
	ModelCatalog bool `json:"model_catalog"`
	Usage        bool `json:"usage"`
	Login        bool `json:"login"`
	BrowserLogin bool `json:"browser_login"`
	PATLogin     bool `json:"pat_login"`
	ImportExport bool `json:"import_export"`
}

type RegionDescriptor struct {
	ID            string         `json:"id"`
	Label         string         `json:"label"`
	ChatBase      string         `json:"chat_base"`
	BillingBase   string         `json:"billing_base"`
	AuthBase      string         `json:"auth_base"`
	DefaultDomain string         `json:"default_domain"`
	Checkin       *CheckinPolicy `json:"checkin,omitempty"`
	// PlanChatBase is the Anthropic Messages base an OAuth/JWT credential
	// talks to (the plan gateway). It differs from ChatBase, which is the
	// pay-as-you-go endpoint an API key is accepted on: the two take
	// different credentials, so a JWT sent to ChatBase is rejected. Empty
	// means the region has no separate plan gateway.
	PlanChatBase string `json:"plan_chat_base,omitempty"`
}

type ProviderDescriptor struct {
	ID                string               `json:"id"`
	Label             string               `json:"label"`
	Runtime           RuntimeKind          `json:"runtime"`
	AuthTypes         []AuthType           `json:"auth_types"`
	CredentialFormats []string             `json:"credential_formats"`
	Capabilities      ProviderCapabilities `json:"capabilities"`
	Regions           []RegionDescriptor   `json:"regions"`
	DefaultRegion     string               `json:"default_region"`
}

func (d ProviderDescriptor) Region(id string) (RegionDescriptor, bool) {
	for _, region := range d.Regions {
		if region.ID == id {
			return region, true
		}
	}
	return RegionDescriptor{}, false
}

func (d ProviderDescriptor) SupportsCredentialFormat(format string) bool {
	for _, f := range d.CredentialFormats {
		if f == format {
			return true
		}
	}
	return false
}

func (d ProviderDescriptor) SupportsAuthType(auth AuthType) bool {
	for _, a := range d.AuthTypes {
		if a == auth {
			return true
		}
	}
	return false
}

// Qoder descriptor. Region selects the pinned CLI and config directory:
// global uses @qoder-ai/qodercli + .qoder, cn uses @qodercn-ai/qoderclicn + .qoder-cn.
var Qoder = ProviderDescriptor{
	ID:                "qoder",
	Label:             "Qoder",
	Runtime:           RuntimeChildProcess,
	AuthTypes:         []AuthType{AuthNone, AuthOAuth, AuthPAT, AuthNative},
	CredentialFormats: []string{"qoder-pat-v1", "qoder-native-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global", ChatBase: "https://api1.qoder.sh",
			BillingBase: "https://openapi.qoder.sh", AuthBase: "https://qoder.sh",
			DefaultDomain: "qoder.global",
			Checkin:       &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
		{
			ID: "cn", Label: "CN", ChatBase: "https://gateway.qoder.com.cn",
			BillingBase: "https://openapi.qoder.com.cn", AuthBase: "https://qoder.com.cn",
			DefaultDomain: "qoder.com.cn",
			Checkin:       &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
	},
	DefaultRegion: "global",
}

// WorkBuddy descriptor. Protocol constants stay in internal/providers/workbuddy;
// the registry only carries control-plane metadata.
var WorkBuddy = ProviderDescriptor{
	ID:                "workbuddy",
	Label:             "WorkBuddy",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthNone, AuthOAuth},
	CredentialFormats: []string{"workbuddy-oauth-v1", "workbuddy-accounts"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: false, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: false, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "cn", Label: "CN", ChatBase: "https://copilot.tencent.com",
			BillingBase: "https://www.codebuddy.cn", AuthBase: "https://copilot.tencent.com",
			DefaultDomain: "codebuddy.cn",
			Checkin:       &CheckinPolicy{Timezone: "Local"},
		},
		{
			ID: "global", Label: "Global", ChatBase: "https://www.workbuddy.ai",
			BillingBase: "https://www.workbuddy.ai", AuthBase: "https://www.workbuddy.ai",
			DefaultDomain: "workbuddy.ai",
			Checkin:       &CheckinPolicy{Timezone: "Local"},
		},
	},
	DefaultRegion: "cn",
}

// Trae descriptor. Protocol constants stay in internal/providers/trae;
// the registry only carries control-plane metadata. CN Solo is the only
// region implemented; do not spawn official traecli.
var Trae = ProviderDescriptor{
	ID:                "trae",
	Label:             "Trae",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth},
	CredentialFormats: []string{"trae-oauth-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: false, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: false, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "cn", Label: "CN Solo", ChatBase: "https://trae-api-cn.mchost.guru",
			BillingBase: "https://api.trae.cn", AuthBase: "https://api.trae.com.cn",
			DefaultDomain: "trae.cn",
			Checkin:       &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
	},
	DefaultRegion: "cn",
}

// Devin descriptor. Protocol constants stay in internal/providers/devin.
// Global-only Connect-RPC adapter; no Devin CLI child process.
var Devin = ProviderDescriptor{
	ID:                "devin",
	Label:             "Devin",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth},
	CredentialFormats: []string{"devin-session-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: false, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global", ChatBase: "https://server.codeium.com",
			BillingBase: "https://api.devin.ai", AuthBase: "https://app.devin.ai",
			DefaultDomain: "devin.ai",
		},
	},
	DefaultRegion: "global",
}

// Command descriptor. Protocol constants stay in internal/providers/command.
// Global-only in-process adapter. Auth is a single pasted user_… Bearer key
// shared by the CLI and the API: no OAuth, no browser loopback. Generation goes
// through /alpha/generate, the CLI's own endpoint, so the $1 Go plan (which has
// no /provider/v1/* API access) is served too.
var Command = ProviderDescriptor{
	ID:                "command",
	Label:             "Command Code",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthPAT},
	CredentialFormats: []string{"command-key-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: false,
		ModelCatalog: true, Usage: true, Login: false, BrowserLogin: false,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global", ChatBase: "https://api.commandcode.ai",
			BillingBase: "https://api.commandcode.ai", AuthBase: "https://api.commandcode.ai",
			DefaultDomain: "commandcode.ai",
		},
	},
	DefaultRegion: "global",
}

// Codex descriptor. OpenAI Codex (ChatGPT subscription) via the ChatGPT
// backend Responses endpoint; OAuth with a fixed loopback redirect.
var Codex = ProviderDescriptor{
	ID:                "codex",
	Label:             "Codex",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth},
	CredentialFormats: []string{"codex-oauth-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: false, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global", ChatBase: "https://chatgpt.com/backend-api/codex",
			AuthBase: "https://auth.openai.com", BillingBase: "https://chatgpt.com",
			DefaultDomain: "chatgpt.com",
		},
	},
	DefaultRegion: "global",
}

// Zhipu descriptor. Protocol constants stay in internal/providers/zhipu.
// CN-only in-process adapter. Auth is a pasted API key; pay-as-you-go and
// Coding Plan differ by base URL, not by a second provider family.
var Zhipu = ProviderDescriptor{
	ID:                "zhipu",
	Label:             "Zhipu GLM",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthPAT},
	CredentialFormats: []string{"zhipu-key-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: false, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: false, BrowserLogin: false,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "cn", Label: "CN", ChatBase: "https://open.bigmodel.cn",
			BillingBase: "https://open.bigmodel.cn", AuthBase: "https://open.bigmodel.cn",
			DefaultDomain: "bigmodel.cn",
		},
	},
	DefaultRegion: "cn",
}

// QwenWork descriptor. Protocol constants stay in internal/providers/qwenwork;
// the registry only carries control-plane metadata. QwenWork is the rebranded
// QoderWork desktop product: global is qwenwork.ai, cn is 千问办公 qwenwork.cn.
// Daily free credits are granted by the server at 00:00 Asia/Shanghai (CN
// wallet copy: "每日 0 点刷新"; global pricing lists the same "daily login
// bonus"), so both regions use that day boundary.
var QwenWork = ProviderDescriptor{
	ID:                "qwenwork",
	Label:             "QwenWork",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth},
	CredentialFormats: []string{"qwenwork-oauth-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: false, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "International", ChatBase: "https://gateway.qwenwork.ai",
			BillingBase: "https://gateway.qwenwork.ai", AuthBase: "https://gateway.qwenwork.ai",
			DefaultDomain: "qwenwork.ai",
			Checkin:       &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
		{
			ID: "cn", Label: "CN", ChatBase: "https://gateway.qwenwork.cn",
			BillingBase: "https://gateway.qwenwork.cn", AuthBase: "https://gateway.qwenwork.cn",
			DefaultDomain: "qwenwork.cn",
			Checkin:       &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
	},
	DefaultRegion: "global",
}

// ZCode descriptor. Accounts sign in to the service the desktop client uses by
// default: Z.ai (chat.z.ai, redirect zcode://zai-auth/callback), whose
// authorize host, callback scheme and API base live in the region below.
var ZCode = ProviderDescriptor{
	ID:                "zcode",
	Label:             "ZCode",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth, AuthPAT},
	CredentialFormats: []string{"zcode-credential-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "zai", Label: "Z.ai",
			ChatBase:     "https://api.z.ai/api/anthropic",
			PlanChatBase: "https://zcode.z.ai/api/v1/zcode-plan/anthropic",
			AuthBase:     "https://zcode.z.ai/api/v1/oauth/token",
			BillingBase:  "https://api.z.ai",
			// The weekend-grab sweep gates on the AutoCheckin opt-in and runs
			// on Beijing calendar edges (the promo window is Beijing Friday
			// 18:00 through Sunday), so the policy zone is Asia/Shanghai even
			// though zcode has no daily check-in of its own.
			Checkin: &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
	},
	DefaultRegion: "zai",
}

// Raccoon descriptor (小浣熊 / SenseTime). In-process provider with two ways in,
// one credential shape:
//   - browser login: the official desktop-login page redirects to a
//     custom-scheme callback (office-raccoon://auth/callback), which only a
//     client that registered that scheme can receive. The console therefore
//     finishes it from a pasted callback URL (BrowserLogin).
//   - credential import / pasted token: the access+refresh token pair the web
//     client already holds, including the desktop client's shared auth.json.
//
// There is no personal access token here; the "PAT" tab pastes the web client's
// access token (or the whole auth.json), which is the same credential shape the
// browser round produces.
var Raccoon = ProviderDescriptor{
	ID:                "raccoon",
	Label:             "Raccoon",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth, AuthPAT},
	CredentialFormats: []string{"raccoon-credential-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global",
			ChatBase: "https://xiaohuanxiong.com/api/web/llm/v2",
			AuthBase: "https://xiaohuanxiong.com/api/web/auth/v1",
			// The daily credit settles on Beijing calendar days (UTC+8
			// midnight), so the schedule and the "already done today" check
			// must both run in that zone.
			Checkin: &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
	},
	DefaultRegion: "global",
}

// Antigravity descriptor (Google's AI IDE). In-process provider with two auth
// modes: a pasted Gemini API key (OpenAI-compatibility layer) and Google
// browser login (Cloud Code Assist / v1internal, the protocol the IDE itself
// speaks). Both modes are first-class; browser login additionally supports a
// pasted callback URL for browsers on another machine.
var Antigravity = ProviderDescriptor{
	ID:                "antigravity",
	Label:             "Antigravity",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth, AuthPAT},
	CredentialFormats: []string{"antigravity-key-v1", "antigravity-oauth-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global",
			ChatBase: "https://cloudcode-pa.googleapis.com/v1internal",
			AuthBase: "https://accounts.google.com/o/oauth2/v2/auth",
		},
	},
	DefaultRegion: "global",
}

// Kimi descriptor. Protocol constants stay in internal/providers/kimi.
// CN-only in-process adapter for the Kimi Code subscription: OAuth device grant
// against auth.kimi.com, distinct from the Moonshot Open Platform API-key
// product that lives on api.moonshot.cn.
var Kimi = ProviderDescriptor{
	ID:                "kimi",
	Label:             "Kimi Code",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth},
	CredentialFormats: []string{"kimi-oauth-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: false, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "cn", Label: "CN", ChatBase: "https://api.kimi.com/coding/v1",
			BillingBase: "https://www.kimi.com", AuthBase: "https://auth.kimi.com",
			DefaultDomain: "kimi.com",
		},
	},
	DefaultRegion: "cn",
}

// CodeArts descriptor. Protocol constants stay in internal/providers/codearts.
// CN-only in-process adapter for Huawei Cloud CodeArts Agent (码道): OAuth PKCE
// against the CodeArts portal mints a short-lived STS trio, and chat goes
// directly to the snap-access OpenAI-compatible gateway. The AK/SK in the
// credential are the STS material, not the account-level IAM keys.
var CodeArts = ProviderDescriptor{
	ID:                "codearts",
	Label:             "CodeArts",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth},
	CredentialFormats: []string{"codearts-oauth-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: false, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: false, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "cn-north-4", Label: "CN",
			ChatBase:      "https://snap-access.cn-north-4.myhuaweicloud.com",
			BillingBase:   "https://snap-access.cn-north-4.myhuaweicloud.com",
			AuthBase:      "https://codearts.huaweicloud.com",
			DefaultDomain: "huaweicloud.com",
			Checkin:       &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
	},
	DefaultRegion: "cn-north-4",
}

// Meta descriptor. Protocol constants stay in internal/providers/meta.
// Global-only Muse (Meta) adapter: RFC 8628 device grant against auth.meta.com,
// minting an LLM API key at api.meta.ai/muse-code/key. Alias "muse" resolves to
// this family at NormalizeProviderFamily / Get; it is not a second provider.
var Meta = ProviderDescriptor{
	ID:                "meta",
	Label:             "Muse (Meta)",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth, AuthPAT},
	CredentialFormats: []string{"meta-oauth-v1", "meta-key-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: false, Login: true, BrowserLogin: true,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global", ChatBase: "https://api.meta.ai/v1",
			BillingBase: "https://www.meta.ai", AuthBase: "https://auth.meta.com",
			DefaultDomain: "meta.ai",
		},
	},
	DefaultRegion: "global",
}

// MuseWeb is the muse.ai web-session adapter: it replays the cookies of a
// logged-in browser tab instead of running an OAuth round, so it needs no
// Muse Code subscription. The protocol constants live in
// internal/providers/muse; keep this descriptor to routing metadata only.
var MuseWeb = ProviderDescriptor{
	ID:                "muse-web",
	Label:             "Muse 网页版",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthNative},
	CredentialFormats: []string{"muse-cookie-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: false, Images: true, Reasoning: false,
		ModelCatalog: true, Usage: false, Login: false, BrowserLogin: false,
		PATLogin: false, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global", ChatBase: "https://muse.ai",
			AuthBase: "https://muse.ai", DefaultDomain: "muse.ai",
		},
	},
	DefaultRegion: "global",
}

// AutoClaw descriptor (智谱 autoglm / OpenClaw desktop client). Two builds
// share one protocol but live on different hosts, so each is its own provider
// family rather than one provider with a region field: the region is a channel,
// and an account's region must be visible in the list (same reasoning as Cline
// and Raccoon). CN keeps the historical id "autoclaw"; the overseas build is
// "autoclaw-intl".
//
// Login is the account plane's own round, not the desktop client's: CN signs in
// with a phone number and an SMS code, Intl with the OAuth redirect the desktop
// client uses. Neither finishes on a socket here (the operator's browser is not
// on this machine), so the round is closed with a pasted callback URL / code.
// This is also the only way in for a Windows install whose auth.json holds a
// DPAPI blob, which cannot be opened off that machine.
var AutoClaw = ProviderDescriptor{
	ID:                "autoclaw",
	Label:             "AutoClaw 国内版",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthPAT},
	CredentialFormats: []string{"autoclaw-credential-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "cn-north-4", Label: "CN",
			ChatBase:      "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw",
			AuthBase:      "https://autoglm-acceleration-api.zhipuai.cn",
			DefaultDomain: "zhipuai.cn",
		},
	},
	DefaultRegion: "cn-north-4",
}

// AutoClawIntl descriptor: the overseas build (autoglm.ai). Its chat path
// carries a /v1 segment the CN build does not, and the site is reachable from
// networks the CN gateway rejects.
var AutoClawIntl = ProviderDescriptor{
	ID:                "autoclaw-intl",
	Label:             "AutoClaw 国际版",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthPAT},
	CredentialFormats: []string{"autoclaw-intl-credential-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: true,
		ModelCatalog: true, Usage: true, Login: true, BrowserLogin: true,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global",
			ChatBase:      "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw",
			AuthBase:      "https://autoglm-api.autoglm.ai",
			DefaultDomain: "autoglm.ai",
		},
	},
	DefaultRegion: "global",
}

// Loomy descriptor (讯飞 Loomy). In-process provider; a server-side login on
// the account service hands back a 14-day session that the OpenAI-compatible
// model gateway accepts as a bearer token.
//
// Three ways in, one credential shape:
//   - credential import: {"loginid","password"} — the adapter logs in and
//     re-logs in by itself, so the account lives as long as the password does;
//   - paste a session token (the "PAT" tab), which works until it expires
//     because there is no refresh token to lean on;
//   - SMS login on the account plane (Login true, BrowserLogin false): there is
//     no browser round, so the console's paste box carries the phone number and
//     then the code.
//
// No check-in: credits refill on the provider's own schedule and the only
// one-off grants are the eight onboarding tasks, which are exposed as claims.
var Loomy = ProviderDescriptor{
	ID:                "loomy",
	Label:             "Loomy 讯飞",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthPAT},
	CredentialFormats: []string{"loomy-credential-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: false,
		ModelCatalog: true, Usage: true, Login: true,
		// BrowserLogin names the console's interactive tab, not a real browser
		// round: the SMS sign-in collects the phone number and then the code in
		// that tab's paste box, the same way AutoClaw's domestic build does.
		// Reporting false would hide the tab and make the round unreachable.
		BrowserLogin: true,
		PATLogin:     true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "cn", Label: "CN",
			ChatBase:      "https://loomyad.xunfei.cn/api/v1",
			AuthBase:      "https://account.xfinfr.com",
			DefaultDomain: "xunfei.cn",
		},
	},
	DefaultRegion: "cn",
}

// Custom descriptor. This is the "bring your own relay" provider: the host is
// not compiled in, it travels in the credential as base_url, so one account is
// one third-party relay plus one key. There is no login and no quota endpoint —
// readiness is "GET {base_url}/models answers 200".
//
// The region exists only because Resolve requires one; it carries no address.
// Runable is the vendor's own web client reached through its cookie. It is a
// chat-only channel: the endpoint takes one flattened prompt and has no tool
// channel, so Tools is false and a request carrying tools gets a plain answer.
// Login is false because this process cannot capture a browser session; PATLogin
// is true because pasting the Cookie header is exactly the supported input.
var Runable = ProviderDescriptor{
	ID:                "runable",
	Label:             "Runable",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthNative},
	CredentialFormats: []string{"runable-cookie-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: false, Images: false, Reasoning: false,
		ModelCatalog: true, Usage: false, Login: false, BrowserLogin: false,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "global", Label: "Global",
			ChatBase:      "https://api.runable.com",
			AuthBase:      "https://api.runable.com",
			DefaultDomain: "runable.com",
		},
	},
	DefaultRegion: "global",
}

// Minimax is MiniMax Code: the mcode CLI's OAuth account talking to the agent
// gateway over Anthropic Messages. The region is part of the credential — a
// pair minted on account.minimax.cn is not accepted by agent.minimax.io — so the
// two sites are two regions of one provider rather than two providers.
//
// Capabilities worth explaining:
//   - Tools is true: the gateway accepts tools and returns tool_use blocks.
//   - Reasoning is false for now even though the catalog advertises thinking
//     variants: the adapter does not send a thinking config yet, and claiming a
//     level it never sends would silently ignore the client's request.
//   - Login/BrowserLogin are true: the device flow has a URL to open, which is
//     what that tab is for, and PollLogin collects the token.
//   - Usage is false: the gateway bills a subscription the public API does not
//     expose.
var Minimax = ProviderDescriptor{
	ID:                "minimax",
	Label:             "MiniMax Code",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthOAuth},
	CredentialFormats: []string{"minimax-oauth-v1"},
	Capabilities: ProviderCapabilities{
		Chat: true, Stream: true, Tools: true, Images: true, Reasoning: false,
		ModelCatalog: true, Usage: false, Login: true, BrowserLogin: true,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{
			ID: "cn", Label: "国内版",
			ChatBase:      "https://agent.minimax.cn",
			AuthBase:      "https://account.minimax.cn",
			DefaultDomain: "minimax.cn",
			Checkin:       &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
		{
			ID: "global", Label: "国际版",
			ChatBase:      "https://agent.minimax.io",
			AuthBase:      "https://account.minimax.io",
			DefaultDomain: "minimax.io",
			Checkin:       &CheckinPolicy{Timezone: "Asia/Shanghai"},
		},
	},
	DefaultRegion: "cn",
}

var Custom = ProviderDescriptor{
	ID:                "custom",
	Label:             "自定义中转站",
	Runtime:           RuntimeInProcess,
	AuthTypes:         []AuthType{AuthPAT},
	CredentialFormats: []string{"custom-relay-v1"},
	Capabilities: ProviderCapabilities{
		// Chat Completions only: compatible relays generally have no
		// /v1/responses, so nothing here claims one. Usage is not exposed
		// either — a relay reports it per response, not on a balance endpoint.
		Chat: true, Stream: true, Tools: true, Images: false, Reasoning: false,
		ModelCatalog: true, Usage: false, Login: false, BrowserLogin: false,
		PATLogin: true, ImportExport: true,
	},
	Regions: []RegionDescriptor{
		{ID: "custom", Label: "自定义"},
	},
	DefaultRegion: "custom",
}

var registry = map[string]ProviderDescriptor{
	Qoder.ID:        Qoder,
	WorkBuddy.ID:    WorkBuddy,
	Trae.ID:         Trae,
	Devin.ID:        Devin,
	Command.ID:      Command,
	Codex.ID:        Codex,
	Zhipu.ID:        Zhipu,
	QwenWork.ID:     QwenWork,
	ZCode.ID:        ZCode,
	Raccoon.ID:      Raccoon,
	Antigravity.ID:  Antigravity,
	Kimi.ID:         Kimi,
	CodeArts.ID:     CodeArts,
	Meta.ID:         Meta,
	MuseWeb.ID:      MuseWeb,
	AutoClaw.ID:     AutoClaw,
	AutoClawIntl.ID: AutoClawIntl,
	Loomy.ID:        Loomy,
	Runable.ID:      Runable,
	Minimax.ID:      Minimax,
	Custom.ID:       Custom,
}

func Get(id string) (ProviderDescriptor, bool) {
	key := strings.TrimSpace(strings.ToLower(id))
	if key == "muse" {
		key = "meta"
	}
	d, ok := registry[key]
	return d, ok
}

func List() []ProviderDescriptor {
	return []ProviderDescriptor{Qoder, WorkBuddy, Trae, Devin, Command, Codex, Zhipu, QwenWork, ZCode, Raccoon, Antigravity, Kimi, CodeArts, Meta, MuseWeb, AutoClaw, AutoClawIntl, Loomy, Custom, Minimax, Runable}
}

// Resolve validates a provider/region pair. Empty values fall back to the
// historical qoder/global default so pre-provider rows keep behaving the same.
func Resolve(provider, region string) (ProviderDescriptor, RegionDescriptor, error) {
	p := strings.TrimSpace(strings.ToLower(provider))
	r := strings.TrimSpace(strings.ToLower(region))
	if p == "" {
		p = Qoder.ID
	}
	descriptor, ok := Get(p)
	if !ok {
		return ProviderDescriptor{}, RegionDescriptor{}, fmt.Errorf("unknown provider %q", provider)
	}
	if r == "" {
		r = descriptor.DefaultRegion
	}
	regionDesc, ok := descriptor.Region(r)
	if !ok {
		return ProviderDescriptor{}, RegionDescriptor{}, fmt.Errorf("unknown region %q for provider %q", region, descriptor.ID)
	}
	return descriptor, regionDesc, nil
}

// ValidateCredentialFormat rejects formats that the provider cannot store.
func ValidateCredentialFormat(provider, format string) error {
	descriptor, _, err := Resolve(provider, "")
	if err != nil {
		return err
	}
	if !descriptor.SupportsCredentialFormat(format) {
		return fmt.Errorf("credential format %q is not supported by provider %q", format, descriptor.ID)
	}
	return nil
}
