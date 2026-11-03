### English

- A model served only by accounts that are momentarily out (cooling down or out of quota) is no longer reported as `model_not_available`. The router now answers `429` with the real wait time — `all accounts that can serve <model> are cooling down; retry in Ns`, or the quota message when the accounts are out of credits. Clients used to see a dead model name and had no reason to retry.
- Added `Pool.UnavailableModelRoute`, which looks past readiness and quota to answer *why* a route is empty. A model that no account serves still returns `model_not_available`, so typos and genuinely absent models are unaffected.

### 中文

- 只有几个正巧在冷却 / 额度耗尽的账号才提供的模型，不再被报成 `model_not_available`。路由现在回 `429` 并带上真实等待时间——`all accounts that can serve <模型> are cooling down; retry in Ns`；若账号是额度用尽则回额度错误。客户端原来只看到一个「模型不存在」，没有任何理由重试。
- 新增 `Pool.UnavailableModelRoute`，越过就绪与额度两个闸门去回答「路由为什么是空的」。没有任何账号提供的模型仍然回 `model_not_available`，写错名字或真没上架的模型不受影响。
