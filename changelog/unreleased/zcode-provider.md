### English

- Add a new `zcode` provider for ZCode (Z.ai / BigModel) Coding Plan accounts. Import accepts a pasted ZCode credential bundle, a plaintext `apiKey` from the official client's `~/.zcode/**/config.json`, a `credentials.json` document, or a bare API key / ZCode JWT; machine-bound `enc:v1:` values are rejected with an actionable message.
- Chat runs against the Anthropic Messages upstream (`https://api.z.ai/api/anthropic/v1/messages` or `https://open.bigmodel.cn/api/anthropic/v1/messages`) and is translated to OpenAI-compatible streaming and non-streaming responses, including tool calls and `reasoning_content` for thinking blocks. Reasoning levels are catalog-driven via `output_config.effort` for the GLM-5.x family.
- Account probe and quota use the ZCode plan-gateway balance endpoint, surfacing remaining balance and plan name on the account card; a 401 marks the account as re-login required. Live model catalogue is fetched from `https://zcode.z.ai/api/v1/client/configs` with a static GLM-5.3 / GLM-5.3-Flash / GLM-5.2 / GLM-5-Turbo fallback.

### 中文

- 新增 `zcode` 渠道，用于接入 ZCode（Z.ai / BigModel）Coding Plan 账号。导入支持粘贴 ZCode 凭证包、官方客户端 `~/.zcode/**/config.json` 中的明文 `apiKey`、`credentials.json` 文档，或直接粘贴 API Key / ZCode JWT；机器绑定的 `enc:v1:` 密文会被拒绝并给出可操作的提示。
- 对话走 Anthropic Messages 上游（`https://api.z.ai/api/anthropic/v1/messages` 或 `https://open.bigmodel.cn/api/anthropic/v1/messages`），翻译为 OpenAI 兼容的流式 / 非流式响应，覆盖工具调用与 thinking 块对应的 `reasoning_content`。GLM-5.x 系列的推理档位由 catalog 驱动，经 `output_config.effort` 透传。
- 账号探活与额度查询使用 ZCode 计划网关的余额接口，在账号卡片展示剩余额度与套餐名；返回 401 时账号会被标记为需要重新登录。模型 catalog 实时取自 `https://zcode.z.ai/api/v1/client/configs`，不可达时回落到内置的 GLM-5.3 / GLM-5.3-Flash / GLM-5.2 / GLM-5-Turbo 列表。
