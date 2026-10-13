# Fork: Antigravity browser login (Google OAuth) + Cloud Code Assist

### English

- Add **Antigravity browser login**: sign in with a Google account through the
  authorization-code + loopback flow. The consent page is opened in a tab, the
  callback lands on this process's own loopback listener
  (`/oauth-callback`), and the minted refresh token is stored as the account
  credential. A callback URL pasted into the console works too, for browsers on
  another machine. `state` is compared byte-for-byte, so a stale link cannot
  complete a newer session.
- Add the **Cloud Code Assist (`v1internal`) plane** for OAuth accounts, which
  is the protocol the Antigravity IDE itself speaks:
  - `POST {base}:streamGenerateContent?alt=sse` for streaming chat, with the
    Gemini SSE envelope translated into chat-completion deltas (thought parts
    become `reasoning_content`, `functionCall` parts become whole `tool_calls`
    deltas, `usageMetadata` folds into chat usage including thoughts).
  - `POST {base}:fetchAvailableModels` for the model catalog, keeping only the
    Gemini family (Claude / GPT-OSS checkpoints are served upstream but this
    adapter does not implement them, so they are not advertised).
  - `loadCodeAssist` → `onboardUser` to discover `cloudaicompanionProject`,
    pinned to the prod host as upstream requires.
  - Three environments (sandbox → daily → prod) are tried in order on 429/5xx.
- **Token renewal**: access tokens are renewed 15 minutes ahead of expiry and
  again after a 401. Renewal is single-flight per account and written back only
  when the stored credential is still the one the refresh started from.
- Keep the existing **pasted Gemini API key** mode working unchanged; the two
  auth modes share one credential type and are told apart by `auth_mode`.

### 中文

- 新增 **Antigravity 浏览器登录**：用 Google 账号走「授权码 + loopback 回调」流程。
  授权页在标签页打开，回调落在本进程自己的 loopback 监听器（`/oauth-callback`），
  拿到的 refresh token 直接落成账号凭证。浏览器在另一台机器时也可以在控制台粘贴
  回调链接完成。`state` 逐字比对，旧链接无法完成新一轮登录。
- 新增 OAuth 账号的 **Cloud Code Assist（`v1internal`）通道** —— Antigravity IDE
  自己用的协议：
  - `POST {base}:streamGenerateContent?alt=sse` 流式推理，Gemini SSE 信封翻译成
    chat-completion 增量（thought 部分映射为 `reasoning_content`，`functionCall`
    映射为一次性完整的 `tool_calls` 增量，`usageMetadata` 折算成 chat usage 并计入
    thoughts）；
  - `POST {base}:fetchAvailableModels` 拉模型目录，只保留 Gemini 家族（上游也下发
    Claude / GPT-OSS，但本适配器未实现，因此不广告）；
  - `loadCodeAssist` → `onboardUser` 自动发现 `cloudaicompanionProject`，按上游要求
    固定走 prod 主机；
  - 三个环境（sandbox → daily → prod）遇 429/5xx 依次轮询。
- **令牌续期**：access token 在到期前 15 分钟续期，401 后再强制续期一次。续期按账号
  单飞，且仅在存储凭证仍是本次续期起点时才回写。
- 原有 **粘贴 Gemini API Key** 模式保持不变；两种鉴权共用一种凭证类型，用 `auth_mode`
  区分。
