// Package loomy implements the Loomy (讯飞 / iFlytek) in-process provider.
//
// Loomy is a desktop AI client whose account plane lives on
// account.xfinfr.com and whose model plane is an OpenAI-compatible gateway on
// loomyad.xunfei.cn/api/v1. A server-side login on the account plane returns a
// 14-day session token, and that token is exactly the bearer the model gateway
// accepts — so the whole provider is "log in once, then speak chat-completions".
//
// # Two planes, two protocols
//
//   - Account plane (account.xfinfr.com): POST /login/account/getPuKey returns a
//     base64 DER RSA public key plus an `rcode` nonce, and
//     POST /login/account/byPwd takes {loginid, password: base64(rsa(pw)), rcode,
//     type: 1, expire}. Passwords never travel in clear. Every request to this
//     host is signed: Authorization: account <akid>:<base64(hmac-sha1(secret,
//     stringToSign))> with the nine-line string-to-sign the official Electron
//     client builds (see signer.go). The same envelope carries an account
//     identity (devid / ua / modelid / version) that the service treats as the
//     device, so it is minted once per account and persisted with the
//     credential instead of being regenerated on every call.
//
//   - Model plane (loomyad.xunfei.cn/api/v1): plain OpenAI chat/completions,
//     plus /models, /points/records, and the onboarding task endpoints. The
//     gateway wants two auth spellings at once (Authorization: Bearer and a bare
//     `token` header) and a `traceparent` header — the reference implementation
//     measured that a request without traceparent hangs until timeout.
//
// # There is no refresh token
//
// A session lives 14 days and there is nothing to refresh it with. The adapter
// therefore stores the login id and password alongside the session and re-logs
// in when the session is gone or upstream answers 401. An account imported
// without a password (session only) cannot renew itself; it turns into an
// ordinary auth failure once the session ages out, which is the honest reading.
//
// # TLS 1.3 does not work here
//
// Measured 2026-11-06 from a US host and again through a Hong Kong SOCKS egress:
// a TLS 1.3 handshake to loomyad.xunfei.cn stalls right after the server's
// ServerKeyExchange and never completes (curl reports HTTP 000 after 20 s),
// while `curl --tlsv1.2` answers 200 in 0.7 s. The model-plane transport is
// therefore pinned to TLS 1.2 and to HTTP/1.1. account.xfinfr.com negotiates
// fine either way, so only the model plane is capped.
//
// # Credits
//
// The unit is 积分 (credits). GET /points/records reports balance /
// dailyBalance / availableBalance, which maps onto QuotaInfo windows. Each
// model carries a `multiplier` — the per-request cost — which is displayed as
// the model's credit rate.
//
// # Onboarding tasks
//
// Eight one-off tasks worth 10,000 credits in total (first message, pick a
// skill, generate a PPT, set a schedule, install a skill, configure remote
// control, create a companion, share it). Completion is judged client-side by
// the official app; the server only records an idempotent flag and credits the
// points, so a bare POST /onboarding/tasks/complete {key} claims them without
// spending a single token. See onboarding.go.
package loomy
