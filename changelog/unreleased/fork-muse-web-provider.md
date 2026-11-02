### English

- Add the `muse-web` provider: it replays a logged-in muse.ai browser session, so Muse is usable without a Muse Code subscription (the `meta` provider still covers Muse Code OAuth). Import accepts an EditThisCookie/Cookie-Editor JSON export, a nested or flat cookie object, a raw `Cookie:` header, a "Copy as cURL" command, or a Netscape `cookies.txt`.
- `muse-web` accounts probe their health with `GET /api/session`, discover the signed-in email for the account label, and expose the `muse-chat`, `muse-image`, `muse-video` catalog. The conversation channel is not calibrated yet: chat returns a clearly-worded 502 instead of a fake success, and the account is not cooled down for it.

### 中文

- 新增 `muse-web` 渠道：直接复用已登录的 muse.ai 网页会话，没有 Muse Code 订阅也能用（`meta` 渠道继续负责 Muse Code 的 OAuth 登录）。导入支持 EditThisCookie / Cookie-Editor 导出的 JSON、嵌套或扁平的 cookie 对象、裸 `Cookie:` 请求头、浏览器「Copy as cURL」命令、以及 Netscape `cookies.txt`。
- `muse-web` 账号用 `GET /api/session` 探活，并把登录邮箱作为账号标签；模型目录提供 `muse-chat`、`muse-image`、`muse-video`。会话通道尚未校准：聊天会明确返回 502 说明原因，而不是假装成功，也不会因此把账号冷却。
