### English

- Two new native providers, at the same level as zcode and trae: accounts are added in the console, and routing, the account pool, health, quota and logs are all the gateway's own. No relay account, no extra container — `minimaxcode2api` and `Gateway-Manager` are references for the protocol, not runtime dependencies.
- **MiniMax Code (`minimax`)**: OAuth device-code login (PKCE S256, public client `mcode-public`), or paste an access token / a `tokens.json`. Upstream is the Anthropic Messages API on `agent.minimax.cn` / `.io`, tool calls work, and the catalog comes from `providers[].config.models`. Refresh tokens are single use, so refreshes go through a per-account single-flight channel, renewal runs hourly 45 minutes early, and a 401 in chat triggers one fallback refresh. System prompts are preserved.
- **Runable (`runable`)**: cookie credential, catalog from the private oRPC call, AI-SDK stream translated to OpenAI SSE. The upstream chat endpoint takes a single prompt and has no tool channel, so `Tools` is false and the vendor model is exposed without a `runable/` route prefix.

### 中文

- 新增两个原生供应商，和 zcode / trae 同级：账号在控制台里添加，路由、账号池、健康分、额度、日志全部由网关自己管。不走中转站账号，不加容器——`minimaxcode2api` 和 `Gateway-Manager` 只是协议参考，不是运行时依赖。
- **MiniMax Code（`minimax`）**：OAuth 设备码登录（PKCE S256，公开客户端 `mcode-public`），也可以直接粘贴 access token 或 `tokens.json`。上游是 `agent.minimax.cn` / `.io` 的 Anthropic Messages 接口，工具调用可用，模型目录来自 `providers[].config.models`。刷新令牌只能用一次，所以刷新走按账号的单飞通道，主循环每小时提前 45 分钟续期，chat 遇到 401 再兜底刷一次。系统提示词保留。
- **Runable（`runable`）**：cookie 凭证，目录走私有 oRPC 调用，AI-SDK 流式翻译成 OpenAI SSE。上游聊天接口只收一个 prompt 且没有工具通道，所以 `Tools` 为 false，该模型不带 `runable/` 路由前缀。
