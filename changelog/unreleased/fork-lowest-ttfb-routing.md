### English

- Add an opt-in `lowest-ttfb` account routing strategy: it remembers each account's smoothed first-byte time and keeps only the accounts whose recent time is close to the pool's best, then rotates inside that band. This targets the reverse-engineered channels (raccoon, zcode, autoclaw) where the same public model answers in 300ms on one account and stalls ten seconds on another - a stall failover cannot see, because the request never fails.
- Unlike hedged requests, it never sends a request twice. Measurement on this deployment showed at least two of those channels keep charging after our side cuts the connection (one cut autoclaw request moved the balance 68 credits, while a completed 16-token request moved it by 1), so racing two accounts would spend credits to save a few seconds. Selecting a faster account costs nothing extra upstream.
- A cold pool routes exactly like round-robin, so enabling the strategy changes nothing until evidence exists; samples expire after ten minutes and a single stall is winsorized at three times the current estimate, so one hiccup cannot exile a healthy account while a sustained slowdown still demotes it. The default stays `round-robin`; switch under System → Account routing strategy or with `PATCH /api/system/settings {"routing_strategy":"lowest-ttfb"}`.

### 中文

- 新增可选调度策略 `lowest-ttfb`：记住每个账号平滑后的首字节耗时，只保留近期耗时接近池内最优的账号，再在这一带内轮询。针对的是逆向渠道（raccoon、zcode、autoclaw）同一个公开模型「A 号 300ms 出字、B 号卡十秒」的情况——failover 看不见这种卡顿，因为请求并没有失败。
- 与竞速对冲不同，它**不会把同一条请求发两次**。本部署实测显示至少两个渠道在我们主动切断连接后仍在扣费（一条被切断的 autoclaw 请求让余额动了 68 积分，而一条完成的上限 16 token 请求只动了 1），所以并发打两个账号等于用积分换几秒。改为优先选更快的账号，上游调用次数不变。
- 冷启动时选号与轮询完全一致，所以在出现证据之前开启这个策略不改变任何流量分布；样本十分钟后失效，单次卡顿按当前估计的三倍封顶，因此一次抖动不会驱逐健康账号，而持续变慢仍会被降级。默认仍是 `round-robin`，在「系统 → 账号调度策略」里切换，或 `PATCH /api/system/settings {"routing_strategy":"lowest-ttfb"}`。
