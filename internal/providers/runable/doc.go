// Package runable is the Runable web provider: one account is a cookie header
// copied out of a signed-in browser, and the chat endpoint answers with an AI SDK
// data stream.
//
// What is specific here:
//
//   - Auth is a session cookie, not a token. There is no refresh: a dead session
//     answers 401 and the account is parked until the operator pastes a new
//     cookie. That is why Login is not advertised — this process cannot capture a
//     browser session by itself — while PATLogin is, because the console's paste
//     box is exactly the right input.
//   - The model list is a POST to an RPC endpoint, wrapped in {"json":{…}}, and
//     it states context/output limits, whether chat mode is enabled and whether a
//     model is free.
//   - The chat call takes ONE prompt string, not a message array. The adapter
//     flattens the conversation, keeps the system prompt verbatim (accounts of
//     this provider must not drop it), and labels assistant/tool turns so the
//     model can tell them apart.
//   - That endpoint has no tool channel. A request carrying tools gets a plain
//     answer, and the catalog says Tools=false so a client can see it up front.
//     The vendor's agent mode lives on a different endpoint (/api/chat/agent)
//     that this adapter does not speak yet.
//
// Public model ids keep the vendor's own spelling, which already contains a slash
// ("anthropic/claude-haiku-5.5", "runable/runner-chat"). No "runable/" prefix is
// registered for route pinning: stripping it would turn the vendor's own
// runable/runner-chat into runner-chat. Pin the provider through an API key's
// provider allowlist instead.
package runable
