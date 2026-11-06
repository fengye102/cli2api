### English

- A quota refusal now fails over to another account that serves the same model instead of ending the request. Quota is per account, so one exhausted account says nothing about the model: before this, a request that happened to land on the exhausted account returned its 429 while a ready sibling sat idle. This is the shape the MiniMax channel has today — the international account has no balance, the domestic one is fine — and the same request used to fail half the time depending on which account was picked.
- The attempt budget is the number of accounts that actually serve the model, so a channel with a single account behaves exactly as before: one attempt, then the quota error. The exhausted account is still cooled to its next quota reset, so it stops being picked.
- A region pin set by the first pick of a request is released on a quota refusal, letting the sibling in the other region answer. A region pinned by a model prefix or by an API-key grant is a routing decision and is never released.

### 中文

- 额度拒绝现在会切到另一台提供同一模型的账号继续，而不是直接结束请求。额度是按账号算的，一台账号耗尽说明不了模型的问题：改动之前，请求只要随机落到耗尽的那台账号上，就会带着它的 429 失败，而旁边还有一台健康的账号闲着。MiniMax 渠道当前就是这个样子——国际版账号没余额、国内版正常——同一个请求会随选到哪台账号时而成功时而失败。
- 重试次数上限取「真正提供该模型的账号数」，所以只有一台账号的渠道行为与之前完全一致：试一次，然后返回额度错误。耗尽的那台账号依旧被冷却到它的额度重置点，不再被选中。
- 请求第一次选号时产生的「区域粘性」在额度拒绝时会解除，让另一个区域的同门账号接手；由模型名前缀或 API Key 授权限定的区域属于路由决定，永远不会被解除。
