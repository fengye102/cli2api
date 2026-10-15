# Fix: Raccoon quota was never fetched (empty stub)

### English

- **Fix the Raccoon (小浣熊) account quota never appearing.** The quota reader was an empty stub that returned a window-less snapshot without ever calling upstream. The runtime treats a window-less snapshot as "not fetched yet" and drops it, so every Raccoon account showed the "no quota data" state permanently, no matter how often the console refreshed.
- The reader now calls the real balance endpoint (`GET /api/web/points/v1/balance`) and reports `available_points` as the credit balance plus the four wallet buckets (daily / monthly membership / reward / top-up). A genuinely zero balance is reported as an honest `0` instead of being indistinguishable from "not fetched".
- Read the subscription tier (`GET /api/web/auth/v1/entitlement_info`, office preferred with a code fallback) as best-effort: a subscription failure no longer fails the balance reading.
- A 401 from a stale stored access token now refreshes once and retries, matching the chat and check-in paths. A balance read that fails is reported as an error so the previous snapshot is kept rather than overwritten with a fabricated zero.

### 中文

- **修复小浣熊账号额度一直不显示的问题。** 额度读取原先是个空实现：不发任何请求，直接返回一个不含窗口的快照。运行时把「不含窗口的快照」视为「尚未获取」并丢弃，于是所有小浣熊账号无论刷新多少次，永远停留在「无额度数据」状态。
- 现在改为真实调用余额接口（`GET /api/web/points/v1/balance`），把 `available_points` 作为可用积分，并同时上报四个钱包分项（每日积分 / 每月会员积分 / 奖励积分 / 充值积分）。余额确实为 0 时会如实显示 0，不再与「尚未获取」混为一谈。
- 订阅信息（`GET /api/web/auth/v1/entitlement_info`，优先办公端、回落代码端）改为「尽力而为」：订阅查询失败不再让整个余额读取失败。
- 存储的 access token 过期（401）时会先刷新一次再重试，与对话、签到链路一致。余额读取失败会如实报错，从而保留上一次快照，而不是用伪造的 0 覆盖。
