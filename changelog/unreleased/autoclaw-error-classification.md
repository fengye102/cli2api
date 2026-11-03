### English

- AutoClaw's 403 is now classified by its message instead of one bucket: `额度已用完 / 开通连续包月会员 / insufficient_quota` becomes **quota** (the account really is out, and the console now says so), while `系统繁忙 / 稍后重试 / too busy` becomes **rate_limit** (the account is fine and the request succeeds seconds later). Both used to be `unavailable`, so an exhausted account failed forever while the pool believed it was healthy, and a busy upstream cooled a healthy account.
- Request-shape rejections from AutoClaw (406 empty body, 400, 422) are now **invalid_request**: the account is not cooled and the request is not failed over. AutoClaw is often a single-account channel, so cooling it turned one malformed request into `no autoclaw accounts available` for every caller behind it.

### 中文

- AutoClaw 的 403 改为按报文分类，不再一律归到一类：`额度已用完 / 开通连续包月会员 / insufficient_quota` 判为 **额度**（账号确实用尽了，控制台也会如实显示），`系统繁忙 / 稍后重试 / too busy` 判为 **限流**（账号没问题，几秒后同样的请求就能成功）。这两类原来都是 `unavailable`：前者会让用尽额度的账号永远失败，而号池仍以为它健康；后者会因为上游一时繁忙把一个好号关进冷宫。
- AutoClaw 的请求形态拒绝（406 空响应体、400、422）改为 **invalid_request**：不冷号、不换号重试。AutoClaw 通常只有一个账号，冷掉它就等于一次请求写错让后面所有调用者都拿到 `no autoclaw accounts available`。
