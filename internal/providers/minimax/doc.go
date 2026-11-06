// Package minimax is the MiniMax Code provider: an account signs in with the
// mcode CLI's OAuth device flow and talks to the agent gateway, which speaks
// Anthropic Messages.
//
// The wire is the same protocol the ZCode plan gateway uses, so framing, tool
// blocks and the OpenAI translation come from internal/providers/anthropicwire.
// What is specific to MiniMax Code:
//
//   - Sign-in is RFC 8628 device authorization with PKCE S256 and the public
//     client "mcode-public". There is no API key to paste; the pair is minted by
//     the flow (or imported from a bridge that already did it).
//   - The refresh token is SINGLE USE. Two refreshes with one token answer
//     invalid_grant and can cost the account its slot, so every refresh goes
//     through a per-account single-flight slot and reuses whatever a concurrent
//     caller already stored.
//   - The request must look like the official client: Bearer token, a
//     placeholder x-api-key, anthropic-version, x-mavis-agent-id and the
//     MiniMaxAgent user agent.
//   - The catalog is a map keyed by model id with real capability metadata
//     (tool_call, context/output limits, input modalities), so tools and window
//     sizes come from the gateway rather than a hardcoded table.
//
// Two sites exist — account.minimax.cn / agent.minimax.cn for the mainland
// deployment and the .io pair for the international one. A token minted on one
// site is not accepted by the other, so the region is part of the credential.
package minimax
