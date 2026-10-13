// Package antigravity implements the Google Antigravity in-process provider.
//
// Antigravity has two auth modes and this adapter implements both:
//
//	key   — a Gemini API key created in Google AI Studio (the documented CLI
//	        path: modelProvider=gemini + GEMINI_API_KEY). Chat, the model list
//	        and probing go through the Gemini OpenAI-compatibility layer
//	        (https://generativelanguage.googleapis.com/v1beta/openai/).
//	oauth — Google browser login. Chat, the model list and project discovery go
//	        through Cloud Code Assist (v1internal), the protocol the Antigravity
//	        IDE itself speaks.
//
// Both modes live in one credential type, told apart by auth_mode (derived from
// which secret is present when the field is absent, so credentials written
// before the field existed still resolve).
//
// # OAuth mode
//
// Login is Google's authorization-code + loopback round: the console opens the
// consent page, Google redirects to this process's own loopback listener, and
// the minted refresh token becomes the account credential. There is no PKCE, so
// `state` is the only one-shot credential on the wire and is compared
// byte-for-byte before the code is spent. A pasted callback URL covers the case
// where the browser runs on another machine.
//
// The refresh token is the long-lived main credential; the access token is an
// hour-scale cache renewed 15 minutes ahead of expiry and again after a 401.
// Google only issues a refresh token on the first consent, so a response that
// omits it leaves the stored value alone rather than blanking it.
//
// Upstream has three planes:
//
//	Google OAuth       accounts.google.com (authorize) / oauth2.googleapis.com
//	                   (token) / www.googleapis.com (userinfo)
//	Cloud Code Assist  <env>-cloudcode-pa.googleapis.com/v1internal — chat,
//	                   catalog and project discovery in one plane
//
// v1internal uses a colon syntax: the base is a path prefix and the method is
// appended with ":", with no /models/{model} segment; the model name travels in
// the request body. Chat relays a Gemini SSE envelope translated into chat SSE.
// The catalog keeps only the Gemini family. Project discovery is pinned to prod
// (the daily host rejects the auth/onboarding calls) and is best-effort: it only
// affects chat, never whether an account can be added.
//
// # Key mode
//
// Chat posts the internal chat request to the OpenAI-compatibility layer's
// /chat/completions and relays the upstream SSE body unchanged; a non-stream
// call collects the same stream. Antigravity has no OpenAI Responses endpoint,
// so /v1/responses stays on the shared chat translation path. The
// Anthropic-compatible endpoint is not used: this gateway already converts
// Anthropic Messages into the internal chat form before the adapter sees them.
//
// # Reasoning
//
// Reasoning follows the Gemini thinking scale (low/medium/high). In key mode it
// is sent as reasoning_effort for gemini-* models and the compatibility layer
// maps it to thinking_level. In OAuth mode it maps onto a thinkingBudget with
// the invariant that maxOutputTokens exceeds the budget. Claude and GPT-OSS
// models keep their upstream thinking behavior; the adapter does not invent an
// effort scale for them.
package antigravity
