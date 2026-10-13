// Package raccoon implements the Raccoon (小浣熊 / SenseTime) in-process
// provider.
//
// Raccoon's web app (xiaohuanxiong.com) exposes an OpenAI-compatible LLM
// gateway. Credentials are JWT-style access tokens with a refresh token;
// access tokens expire, and the adapter refreshes them through the auth
// gateway. Model catalog is a static fallback list plus a live
// GET /model_catalog refresh.
package raccoon

// Version is not pinned to a client build: the web gateway accepts any
// request that carries a valid bearer token.

// LLM gateway base for chat and catalog.
const llmBaseURL = "https://xiaohuanxiong.com/api/web/llm/v2"

// Auth gateway base for token refresh.
const authBaseURL = "https://xiaohuanxiong.com/api/web/auth/v1"

// chatPath is the OpenAI-compatible chat endpoint under the LLM base.
const chatPath = "/chat/completions"

// catalogPath is the model catalog endpoint under the LLM base.
const catalogPath = "/model_catalog"

// refreshPath is the token refresh endpoint under the auth base.
const refreshPath = "/refresh"

// catalogCacheTTL bounds how long a fetched model catalog is reused.
const catalogCacheTTL = 10 * 60 // seconds

// probeTimeout keeps liveness probes short.
const probeTimeoutSeconds = 10
