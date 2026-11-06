// Package custom is the "bring your own relay" provider: one account is a base
// URL plus an API key, and whatever OpenAI-compatible models live behind that
// URL become usable through the gateway.
//
// It exists because every other provider in this tree has its host compiled into
// registry.go, so a third-party relay — 中转站 — could not be added without a
// code change. Here the address travels in the credential instead:
//
//	{"format":"custom-relay-v1","base_url":"https://relay.example.com/v1","api_key":"sk-…"}
//
// Deliberate differences from the other adapters:
//
//   - No login. The key is the whole credential; there is nothing to refresh and
//     no browser step. Prober.Probe reads /models to prove the key works.
//   - No request rewriting beyond `model`, `stream` and the developer→system
//     role swap. A relay is by definition a passthrough; second-guessing its
//     payload shape is how compatibility bugs get introduced.
//   - Chat Completions only. sub2api learned this the hard way and documents it
//     in openai_compat/upstream_capability.go: compatible upstreams generally do
//     not implement /v1/responses, so the gateway would 404 on every call if it
//     translated CC→Responses. This adapter therefore never sends a request to
//     /v1/responses.
//   - The model list comes from the upstream (GET {base}/models) with a TTL, and
//     a credential may carry `models` and `model_mapping` for relays whose list
//     endpoint is missing, broken or incomplete.
//
// Public model ids stay unprefixed (the id the relay published), so a client can
// ask for the name it already knows; `custom/<model>` also works and pins the
// route to this provider.
package custom
