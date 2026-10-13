// Package antigravity implements the Google Antigravity in-process provider.
//
// Antigravity authenticates headlessly with a Gemini API key created in Google
// AI Studio (the documented CLI path: modelProvider=gemini + GEMINI_API_KEY).
// This adapter stores that key as the account credential and talks to the
// Gemini OpenAI-compatibility layer
// (https://generativelanguage.googleapis.com/v1beta/openai/), so chat, the
// model list, and probe all speak one authenticated protocol. There is no
// browser login and no child process.
//
// Chat posts the internal chat request to /chat/completions and relays the
// upstream SSE body unchanged; a non-stream call collects the same stream.
// Antigravity has no OpenAI Responses endpoint, so /v1/responses stays on the
// shared chat translation path. The Anthropic-compatible endpoint is not used:
// this gateway already converts Anthropic Messages into the internal chat form
// before the adapter sees them.
//
// Reasoning follows the Gemini thinking scale (low/medium/high), sent as
// reasoning_effort only for gemini-* models; the OpenAI-compat layer maps it to
// thinking_level. Claude and GPT-OSS models served by Antigravity keep their
// upstream thinking behavior and the adapter does not invent an effort scale
// for them.
package antigravity
