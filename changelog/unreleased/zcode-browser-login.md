### English

- The `zcode` channel now ships the domestic service only (BigModel / `bigmodel.cn`): one region, one authorize flow. A credential minted for the international service (`zai`, `z.ai` / `chat.z.ai`) is rejected at import with an actionable message instead of being silently pointed at the wrong upstream.
- Add browser login for zcode accounts. The console opens `https://bigmodel.cn/login?redirect=zcode://oauth/callback&appId=zcode&state=<state>` in a browser tab and the operator pastes the callback URL back: the redirect target is a `zcode://` custom scheme that only the official desktop client can receive, so no server-side loopback can complete it and pasting is the entire flow. The pasted code is exchanged at `https://zcode.z.ai/api/v1/oauth/token` for the ZCode JWT plus the provider access/refresh tokens, which are stored on the account; the account identity (email, user id) is filled best-effort from the BigModel customer endpoint.
- The login round is bound to a 32-byte `state` minted per start and expires after 15 minutes, so a callback link from an earlier round cannot complete a newer one; the pasted URL is validated (scheme, callback host, `code`/`authCode`, provider `error`) before any exchange happens, and a rejected exchange leaves the account untouched. Pasting an API key is exposed as the PAT login method alongside it.

### 中文

- `zcode` 渠道改为只提供国内版服务（BigModel / `bigmodel.cn`）：单区域、单授权流程。国际版（`zai`，`z.ai` / `chat.z.ai`）的凭证在导入时就会被拒绝并给出可操作提示，不再被静默指到错误的上游。
- 新增 zcode 浏览器授权登录。控制台在浏览器标签页打开 `https://bigmodel.cn/login?redirect=zcode://oauth/callback&appId=zcode&state=<state>`，用户把浏览器跳转后的回调链接粘回来：回调目标是 `zcode://` 自定义协议，只有官方桌面客户端能接收，服务器侧不可能自动回调，所以「粘贴回调链接」就是完整流程。粘贴的 code 会去 `https://zcode.z.ai/api/v1/oauth/token` 换取 ZCode JWT 与提供方 access/refresh token 并落库，账号身份（邮箱、用户 ID）尽力从 BigModel 客户信息接口补齐。
- 每次登录都会新生成 32 字节 `state` 且 15 分钟后过期，因此上一轮的回调链接无法完成新一轮登录；粘贴的链接在真正换取 token 之前会先校验（协议、回调 host、`code`/`authCode`、上游 `error` 参数），换取失败不会污染账号。同时把「粘贴 API Key」作为 PAT 登录方式一起开放。
