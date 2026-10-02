### English

- The `zcode` channel now signs in to one service only: Z.ai (`chat.z.ai`, redirect `zcode://zai-auth/callback`). The BigModel (bigmodel.cn) region is gone, so the account-type list, the credential validator and the browser-login realms no longer carry a second service. A credential that still names `bigmodel` is rejected instead of being sent somewhere it cannot authenticate.
- Account type and account cards read `ZCode Z.ai` (the "(international)" qualifier is gone), and zcode accounts show the ZCode client mark instead of falling back to the Qoder logo.
- System settings lists the upstream version history for a fork or development build: the running build stays first and no update is offered, but the official releases are listed instead of an empty panel.
- API access page: the Anthropic-compatible endpoint gets its own base URL card. Anthropic clients append `/v1/messages` themselves, so they need the origin (no `/v1` suffix) — the OpenAI base still carries `/v1`.

### 中文

- `zcode` 渠道现在只登录一个服务：Z.ai（`chat.z.ai`，回调 `zcode://zai-auth/callback`）。BigModel（bigmodel.cn）区域已移除，账号类型列表、凭证校验与浏览器授权都不再带第二个服务；仍旧写着 `bigmodel` 的凭证会被拒绝，而不是被发到它根本认证不了的地方。
- 账号类型与账号卡片显示为 `ZCode Z.ai`（去掉「（国际版）」标注），且 zcode 账号显示 ZCode 客户端图标，不再回落到 Qoder 的图标。
- 系统设置在 fork/开发版构建下也能列出官方版本历史：当前运行的构建排在第一，不提示更新，但会列出官方各个版本，而不是一个空面板。
- API 访问页：Anthropic 兼容端点单独给一张 Base URL 卡片。Anthropic 客户端会自己拼 `/v1/messages`，所以要填根地址（不带 `/v1`）；OpenAI 的 Base URL 仍然带 `/v1`。
