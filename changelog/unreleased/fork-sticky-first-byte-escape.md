### English

- Let a session leave an account that has gone slow. Session affinity is decided before account selection runs, so on this deployment 92.3% of requests never reached the latency band at all: one account averaging 19.4s first byte held 128 of its 134 requests through affinity, and a bound session stayed there until the one-hour TTL expired. When a binding's recent first byte is more than two seconds behind a ready alternative, the request now competes normally instead of being pinned, and the successful attempt re-binds the session to whatever actually answered.
- The escape is deliberately conservative: both the bound account and the alternative need a fresh sample, the alternative must be one the picker would accept (same route, not cooling, not saturated), and the gap must exceed two seconds. A small gap keeps the binding, because cache locality and stable behaviour are worth more than a few hundred milliseconds.
- It only applies under the `lowest-ttfb` strategy. Round-robin, weighted, fill-first and FEFO keep their existing behaviour even though latency samples are collected, so switching strategies never silently reshuffles conversations. Escape reasons are counted as `slow_first_byte` alongside the existing `account_cooldown` and `model_unavailable` ones.

### 中文

- 允许会话离开已经变慢的账号。会话粘性在选号之前就决定了，所以在这台部署上 **92.3% 的请求根本没走到选择带**：一个首字节均值 19.4s 的账号靠粘性吃下了自己 134 条请求里的 128 条，绑定的会话要一直卡到一小时 TTL 到期。现在只要绑定账号的近期首字节比某个可用备选慢两秒以上，这条请求就不再被 pin，而是正常参与选择；成功后会话会重绑到真正应答的账号。
- 逃逸刻意保守：绑定账号与备选都**必须有新鲜样本**，备选必须是选号器认可的候选（同路由、不在冷却、并发未打满），且差距要超过两秒。差距小的保持绑定——缓存局部性和行为稳定的价值高于几百毫秒。
- 该逃逸只在 `lowest-ttfb` 策略下生效。轮询、加权、填满优先、FEFO 即便采集了延迟样本也保持原有行为，切换策略不会悄悄搬动会话。逃逸原因记为 `slow_first_byte`，与既有的 `account_cooldown`、`model_unavailable` 并列统计。
