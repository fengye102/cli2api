### English

- **Fix imported AutoClaw credentials never refreshing on their own.** Importing a credential stored the tokens but not `expires_at`. An empty expiry is deliberately treated as "the upstream is the authority", so `NeedsRefresh()` returned false forever and the account never refreshed proactively — it survived only until the 24h access token ran out, then died silently and needed a manual re-login.
- The import path now derives `expires_at` from the access token's own `exp` claim, so a freshly added account refreshes inside the 5-minute window like any other provider. An explicitly supplied `expires_at` still wins, so re-importing never rewrites a deliberately overridden value.
- Credentials already stored without an expiry get the same value backfilled on read, so accounts added before this fix start refreshing without being re-imported.
- Regression tests: `TestImportDerivesExpiresAt`, `TestImportKeepsExplicitExpiresAt`, `TestCredentialBackfillsExpiryFromToken`.

### 中文

- **修复导入的 AutoClaw 凭证从不自动刷新。** 导入时保存了 token 却没有保存 `expires_at`。空的过期时间按设计会被当成「以上游为准」，于是 `NeedsRefresh()` 永远返回 false，账号从不主动刷新——它只能撑到 24 小时的 access token 用完，随后静默失效、必须人工重新登录。
- 导入路径现在会从 access token 自身的 `exp` 声明推导 `expires_at`，新加的账号因此和其他渠道一样在到期前 5 分钟内自动刷新。显式传入的 `expires_at` 仍然优先，重复导入不会覆盖被刻意改过的值。
- 已经存在但没有过期时间的凭证会在读取时补上同一个值，所以本次修复之前添加的账号无需重新导入就能开始刷新。
- 回归测试：`TestImportDerivesExpiresAt`、`TestImportKeepsExplicitExpiresAt`、`TestCredentialBackfillsExpiryFromToken`。
