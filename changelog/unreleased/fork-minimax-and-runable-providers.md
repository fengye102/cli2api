# 新增原生供应商：MiniMax Code 与 Runable

两个仓库的能力不再走"中转站账号"或额外容器，而是作为 cli2api 自己的 provider
实现，和 zcode / trae 同级：账号在控制台里添加，路由、账号池、健康分、日志全部由
网关自己管，运行时不依赖 minimaxcode2api 或 Gateway-Manager。

## MiniMax Code（`provider=minimax`）

- 登录：mcode CLI 的 OAuth 设备码流程（PKCE S256，公开客户端 `mcode-public`），
  控制台点"浏览器登录"会给出验证链接和用户码，轮询拿到 token 后写入凭证。
  也可以直接粘贴 access token，或粘贴 minimaxcode2api 的 `tokens.json`。
- 上游：`agent.minimax.cn` / `.io` 的 Anthropic Messages 接口，工具调用（tool_use）
  可用，模型目录是 `{providers[].config.models}` 这种以模型 id 为键的映射，
  上下文窗口、输出上限、tools/reasoning 能力都从目录里读。
- **单次 refresh token**：刷新令牌只能用一次，用第二次会 `invalid_grant` 并可能
  废掉账号。因此刷新走按账号的单飞通道，并发 401 只会花掉一个刷新令牌；凭证里
  同时保存 access/refresh 与到期时间，主循环每小时提前 45 分钟续期，chat 遇到 401
  再兜底刷新一次。
- 系统提示词保留（`system` 走 Messages 顶层字段）。

## Runable（`provider=runable`）

- 凭证：网页 cookie（`runable-cookie-v1`），粘贴 `Cookie:` 头即可；没有刷新机制，
  会话失效就是 401，账号按鉴权冷却 30 分钟等人工换 cookie。
- 目录：`POST /api/rpc/public/models/getAIChatModels`，返回 16 个模型（含 8 个免费、
  多个 1M 上下文），带 `context_window` / `max_tokens` / `isFree`，读取后缓存 10 分钟。
- 聊天：`POST /api/chat/text` 收的是**一段扁平化后的提示词**，不是消息数组。适配器
  保留 system 原文、给 assistant/tool 轮加前缀、图片按"文本端点不支持"标注，再把
  AI SDK 数据流（`text-delta` / `textDelta` / 自定义 usage 帧 / `finish`）翻译成
  OpenAI SSE。
- 该端点没有工具通道，目录里 `Tools=false`；带 tools 的请求会得到纯文本回答。
  厂商的 agent 模式在另一个端点（`/api/chat/agent`），本适配器暂未接入。
- 模型 id 保留厂商原样（`runable/runner-chat`、`anthropic/claude-haiku-5.5`），
  因此**没有**注册 `runable/` 路由前缀——否则会把厂商自己的 `runable/…` 削成别的模型；
  需要钉住供应商时用 API Key 的 provider 白名单。

## 共享层

`internal/providers/anthropicwire` 收拢了 Anthropic Messages 的请求体构造、
SSE→OpenAI 翻译和非流式聚合，ZCode 改为薄封装（对外行为不变，原有测试全部保留）。

## 其它

- `internal/executor/prepare.go`：新增 `minimax/` 路由前缀。
- `accounts.DefaultDropSystemPrompt`：minimax、runable 加入保留 system 的名单。
- 控制台：新增 MiniMax / Runable 图标与中英文标签，不再回退成 Qoder logo。
