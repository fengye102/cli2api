### English

- zcode: a spent or ended Start Plan trial now rests that trial entry, not the account. A trial is its own catalogue entry (`<model>-Trial`) with its own spend, and the gateway refuses it alone — `400 "ZCode has no active limited quota for GLM-5.3-Trial"` when the trial ended, or a 429 naming the bucket (`GLM-5.3`, the entry less `-Trial`) when the spend is gone. Neither wording matched the quota signals before (the 400 read as a client error and cooled nothing, the 429 as a plain rate limit and retried into the same refusal), and a quota cooldown is account-wide until local midnight, so one spent trial paused the plan's paid GLM-5.3 for the rest of the day. A provider may now scope a quota refusal to one model, and the pool honours it for quota as it already did for rate limits: the trial entry is rested, everything else on the account keeps serving. An account-level balance or another model's bucket still rests the whole account.

### 中文

- zcode：Start Plan 试用额度用尽或试用结束时，只冷却那个试用条目，不再整号冷却。试用在目录里是独立条目（`<model>-Trial`），有独立的额度桶，网关也只会单独拒它——试用结束是 `400 "ZCode has no active limited quota for GLM-5.3-Trial"`，桶花完是 429 并点名桶（`GLM-5.3`，即条目去掉 `-Trial`）。这两种说法原来都不在配额信号里（400 被当成客户端错误、什么都不冷却；429 被当成普通限流，反复重试同一个拒绝），而配额冷却又是整号到本地零点，于是试用一花完，套餐里付费的 GLM-5.3 也跟着停一整天。现在 provider 可以把配额拒绝收窄到单个模型，pool 对配额也像对限流一样尊重这个范围：只冷却那个试用条目，账号其他模型照常服务。账号级余额不足或别的模型的桶，仍然整号冷却。
