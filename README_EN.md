<div align="center">

<h1><img src="./frontend/public/apple-touch-icon.svg" width="40" height="40" align="top" alt=""> CLI2API</h1>

**Turn your own logins into a local OpenAI-compatible API**

Connect **Qoder (Global / CN)**, **WorkBuddy (Global / CN)**, **Trae CN Work**, **ZCode (Z.ai)**, and experimental **Devin** and **Command Code** accounts.

Deploy with Docker, manage accounts in the web console, and connect clients through compatible APIs.

[![License](https://img.shields.io/github/license/caigee-cmd/cli2api)](LICENSE)
[![LINUX DO](https://img.shields.io/badge/LINUX%20DO-community-ff6a00)](https://linux.do)

<sub>[中文](README.md) · [Issues](https://github.com/fengye102/cli2api/issues) · [LINUX DO](https://linux.do)</sub>

<img src="./docs/assets/readme/hero-en.svg" width="100%" alt="CLI2API — turn your own logins into a local OpenAI-compatible API">

</div>

> **This repository is a fork of [caigee-cmd/cli2api](https://github.com/caigee-cmd/cli2api)**: it adds a **ZCode (Z.ai)** channel (browser login, credential import, and the plan-channel verification solver), fixes several credential-import bugs, and polishes a few console details. See “Differences from upstream” below for the full list.

## Features

- **Compatible APIs**: Chat Completions, Responses, Anthropic Messages, and model listing, with streaming and function tools.
- **Multi-account routing**: automatic selection, session affinity, concurrency limits, cooldowns, and failover.
- **Web console**: manage accounts, models, client keys, and proxies; view quotas and request logs.
- **Docker operations**: persistent account data; an optional host updater lets you download, confirm, and roll back updates from the console.
- **ZCode (Z.ai) channel**: browser sign-in (the ZCode CLI flow or a pasted callback URL) and credential import; chat runs against the Anthropic Messages upstream and is translated to OpenAI-compatible responses, including tool calls and reasoning levels. The plan channel needs the companion verification solver — see [`deploy/captcha_node`](deploy/captcha_node/README.md).

Qoder CN, WorkBuddy, and Trae adapters are implemented, but live-account acceptance is still pending. Devin and Command Code are experimental, not production-ready. Live upgrade and rollback acceptance is also pending.

## Quick start

You need Docker and an upstream account you control. Use Docker Desktop on macOS / Windows; Windows must use Linux containers.

```bash
git clone https://github.com/fengye102/cli2api.git
cd cli2api
./scripts/start.sh        # Windows: scripts\start.ps1
```

1. Save the **administrator key** printed in the first-start logs.
2. Open `http://127.0.0.1:3010`, sign in with that key, and add an account in **Accounts**.
3. Create a client key in **API keys**, then choose a model and test it in **Access**.

Docker Compose is the supported install and managed-update path; source runs are for development. See the [deployment guide](deploy/README.md).

## Connect a client

In an OpenAI-compatible client, enter:

```text
Base URL: http://127.0.0.1:3010/v1
API Key:  <a client key created in API keys>
```

Get a model ID from **Access** or `/v1/models`. Routing is automatic and prefers the same account for later turns. Account pinning, session headers, and curl / PowerShell examples are in the [deployment guide](deploy/README.md).

## How it works

<p align="center">
  <img src="./docs/assets/readme/architecture-en.svg" width="100%" alt="CLI2API architecture: OpenAI clients are routed by the Go control plane to one isolated runtime per account, then to the provider upstream">
</p>

The Go gateway handles authentication, routing, and request logging. Each Qoder account has its own Node process and HOME; WorkBuddy, Trae, ZCode, Devin, and Command Code use Go in-process adapters. No full CLI is started per request.

## Console

<p align="center">
  <img src="./docs/assets/readme/console-window-en.svg" width="100%" alt="CLI2API console Accounts page: each account shows its login method, ready state, and quota, with an Access panel offering the Base URL and a quick check">
</p>

Accounts, models, access, and logs all live in one web console: readiness and quota are visible at a glance, and the Access page lets you copy the Base URL and run a quick check.

Account cards provide WorkBuddy, Qoder CN, and Qoder Global check-in actions and automatic check-in switches (off by default), with history under More → Check-in records; no separate check-in page is needed. Set each provider's default time under System → Automatic check-in, then choose inheritance or an override in Edit account. Default changes apply to inheriting accounts immediately. Existing WorkBuddy accounts retain their previous times until switched to inheritance. Inactive campaigns are skipped. Live-account check-in acceptance is still pending.

## Limitations

- Bring your own accounts. CLI2API does not supply accounts, quotas, or an official API service.
- Compatibility is not full API parity. Messages / Responses are stateless adapters, without server-side conversations or upstream-specific tool execution.
- Image support depends on the upstream and model. WorkBuddy / Trae do not support images; file inputs are rejected. See the [deployment guide](deploy/README.md) for details.
- Cross-provider routing for shared model IDs follows system settings, key grants, and region constraints; failover is not unrestricted.
- Upstream changes can break compatibility. Request logs do not store prompt or completion bodies by default.

## Differences from upstream

This fork builds on [caigee-cmd/cli2api](https://github.com/caigee-cmd/cli2api) and adds:

- **ZCode (Z.ai) channel**: use a ZCode client login (Z.ai Coding Plan) to reach the GLM family.
  - Login and import: browser sign-in (the ZCode CLI flow, or a pasted callback URL as fallback), a pasted API key (PAT), and several credential import shapes — a ZCode credential bundle, the plaintext `apiKey` in the official client's `~/.zcode/**/config.json`, a `credentials.json` document, or a bare API key / JWT. Machine-bound `enc:v1:` values are rejected with an actionable message.
  - Chat runs against the Anthropic Messages upstream and is translated to OpenAI-compatible streaming and non-streaming responses, including tool calls and `reasoning_content` for thinking blocks; reasoning levels are catalog-driven.
  - **The plan channel needs the verification solver**: it rejects a request without a fresh Aliyun verification token, so deploy `deploy/captcha_node` (Chromium plus `npm install` in that directory) and point `ZCODE_CAPTCHA_DIR` / `ZCODE_CHROMIUM_PATH` at it. Without it, plan-channel chat is rejected upstream with code 3012.
  - Only **Z.ai** is supported (the BigModel region is gone).
- **Credential import fixes**: candidate fields are resolved in a deterministic, caller-defined priority order (previously map iteration order could silently pick a different account from the same paste), `uid+token` values keep the `+` inside the token, and `{account, auth}`-style nested exports are only parsed that way when they really carry an access token.
- **Console details**: zcode accounts show `ZCode Z.ai` and the vendor mark; the API access page gives the Anthropic-compatible endpoint its own origin-only base URL card (Anthropic clients append `/v1/messages` themselves, so a `/v1`-suffixed base becomes `/v1/v1/messages`); and system settings list the official version history even on a fork or development build.
- **Extra WorkBuddy models**: inject models outside the Grayscale catalog with `WORKBUDDY_EXTRA_MODELS` (the default injects `deepseek-v4.1-flash`, see `deploy/docker-compose.yml`).

Everything else behaves and deploys like upstream; the upstream documentation and release notes still apply.

## Documentation

- [Deployment and operations: setup steps, environment variables, endpoints, managed updates](deploy/README.md)
- [Changelog](CHANGELOG.md)

## Security

The default deployment exposes only `127.0.0.1:3010`; do not expose it directly to the public internet. Administrator keys manage the console; client keys only access `/v1/*`. Protect keys, credential exports, and database backups. Report security issues privately as described in [SECURITY.md](SECURITY.md).

## Community & Contributing

Chinese-language discussion is on [LINUX DO](https://linux.do). Bugs and feature requests go to GitHub [Issues](https://github.com/fengye102/cli2api/issues); documentation improvements and pull requests are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).

## Acknowledgements

Thanks to open-source projects including `workbuddy2api`, `CLIProxyAPI`, and `sub2api` for ideas and inspiration.

## License

[MIT](LICENSE). Follow each upstream platform's terms when using its accounts.
