### English

- The MiniMax Code account card shows the real credit balance now. The account exposes no aggregate balance endpoint, only the list of granted credit packs (`/minimax-cloud/api/v1/credit/details`), so the adapter sums them: total granted, remaining, consumed, the soonest expiry with the credits that die there, and every pack with its own end date.
- An account with no packs reads as exhausted instead of merely ready. The international account has zero credits, and its next chat request used to be the thing that discovered it — with the 1008 refusal. Routing now skips it before the request.
- A provider that exposes no quota surface no longer claims to be fetching one: the account card says the balance is not tracked instead of leaving "fetching quota…" on screen forever. Runable is in that group — its web app reads the balance over an oRPC contract no request shape we tried would answer, and inventing a number is worse than saying so.

### 中文

- MiniMax Code 的账号卡片现在显示真实的积分余额。该账号没有聚合余额接口，只有发放包列表（`/minimax-cloud/api/v1/credit/details`），所以适配器自己汇总：总发放、剩余、已消耗、最早到期时间与在该时刻作废的积分，以及每个包各自的到期日。
- 没有发放包的账号会被判定为「额度耗尽」而不是「就绪」。国际版账号积分是 0，以前要靠下一次对话撞上 1008 拒绝才发现；现在路由会在请求前就跳过它。
- 没有额度接口的供应商不再假装正在拉取：账号卡片直接显示「未跟踪余额」，而不是让「额度获取中…」永远挂着。Runable 属于这一类——它的网页端通过一套 oRPC 契约读取余额，试过的请求形状一律 404，编一个数字出来比说实话更糟。
