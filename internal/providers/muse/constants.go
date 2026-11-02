package muse

import (
	"strings"
	"time"
)

// ProviderID is the canonical provider family id. It is deliberately distinct
// from "meta": meta talks to Meta's Muse Code OAuth/API-key endpoints
// (auth.meta.com + api.meta.ai), while this provider replays a logged-in
// muse.ai web session, which needs no subscription and no minted key.
//
// The id is "muse-web", not "muse": the registry already resolves the alias
// "muse" to the meta family (see registry.Get / NormalizeProviderFamily), so a
// provider literally named "muse" would be shadowed by that alias and never
// dispatched.
const (
	ProviderID    = "muse-web"
	ProviderLabel = "Muse 网页版"

	// CredentialFormat is the on-disk payload format. It holds the browser
	// cookies that make up a logged-in muse.ai session.
	CredentialFormat = "muse-cookie-v1"
)

// Upstream facts, all observed from muse.ai's public web client. Everything
// that a site update could invalidate lives in this file so a breakage is a
// one-file fix.
const (
	SiteURL      = "https://muse.ai"
	SessionAPI   = SiteURL + "/api/session"
	VMWakeAPI    = SiteURL + "/api/hatch/vm/wake"
	NewThreadURL = SiteURL + "/thread/new"

	// UserAgent matches the desktop Chrome the web client expects. muse.ai
	// serves its agent UI to a normal desktop UA; a Go UA gets the marketing
	// page.
	UserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

// Session cookies. muse.ai stopped setting hatch_gw / hatch_vml for every
// logged-in session in 2026-10, so they are forwarded when present but never
// required.
var (
	// requiredCookies must be present for a credential to be usable.
	requiredCookies = []string{"hatch_sess"}
	// recommendedCookies are needed by most (but not all) sessions; a missing
	// one is reported as a warning instead of rejecting the import.
	recommendedCookies = []string{"hatch_native_auth_device"}
	// optionalCookies are forwarded but never required and never warned about.
	optionalCookies = []string{"hatch_gw", "hatch_vml"}
)

const (
	// sessionProbeTimeout bounds the /api/session health check.
	sessionProbeTimeout = 20 * time.Second
	// wakeConfirmTimeout bounds how long a VM wake is polled before giving up.
	wakeConfirmTimeout = 20 * time.Second
)

// Model ids exposed on the gateway. muse.ai's web client has one assistant,
// one image model and one video model; the names below follow muse2api's
// public contract so existing OpenAI tooling works unchanged.
const (
	ModelChat  = "muse-chat"
	ModelImage = "muse-image"
	ModelVideo = "muse-video"
)

// modelAliases maps popular client-side model names onto the three muse
// models, mirroring muse2api's ALIASES table.
var modelAliases = map[string]string{
	"gpt-4o":          ModelChat,
	"gpt-4o-mini":     ModelChat,
	"gpt-4.1":         ModelChat,
	"gpt-5":           ModelChat,
	"claude-sonnet-4": ModelChat,
	"deepseek-chat":   ModelChat,
	"dall-e-3":        ModelImage,
	"gpt-image-1":     ModelImage,
	"sora":            ModelVideo,
}

// ResolveModel maps a client-supplied model id onto a muse model id. Unknown
// chat names fall back to ModelChat, which is what muse2api does.
func ResolveModel(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ModelChat
	}
	if alias, ok := modelAliases[name]; ok {
		return alias
	}
	switch name {
	case ModelChat, ModelImage, ModelVideo:
		return name
	}
	return ModelChat
}
