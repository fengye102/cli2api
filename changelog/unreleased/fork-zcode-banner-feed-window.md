### English

- zcode: the weekend sweep reads the ZCode app's claim banners (`GET /api/v1/marketing/touch?seq=n`, each delivery's `claim_zcode_plan` button) before falling back to the date-encoded candidate ids, so a promo whose plan id cannot be predicted is claimed instead of missed. A banner plan is in neither billing list while the banner is up — the `ZCODE WEEKEND BUILD` banner of 10-30 was in no billing view at all — so the date guess alone was chasing an id that may not exist. A feed that does not read is named in the round's message and never reads as "nothing to claim", and the date guesses carry that round.
- zcode: the sweep now stays quiet outside the promo window (Friday 18:00 through Sunday midnight, Beijing). It was posting a claim every ten minutes all week; upstream answers those with 1005 and, once they pile up, with a bare `429` that the round reported as an empty body. The window check runs before the round's throttle is stamped, so the first tick after the window opens fires at once. A claim answered `429` also says so now instead of "empty upstream body".

### 中文

- zcode：周末抢领先读 ZCode 客户端的领奖横幅（`GET /api/v1/marketing/touch?seq=n`，每个 delivery 的 `claim_zcode_plan` 按钮），读不到再退回按日期猜的候选 id，这样 plan_id 猜不中的活动也能领到。横幅上的套餐在横幅挂着的时候两侧账单列表里都没有——10-30 的「ZCODE WEEKEND BUILD」横幅在任何一个 billing 视图里都查不到——只靠日期猜等于在追一个可能不存在的 id。feed 读不出来会写进本轮消息，绝不当作「没有可领的」，那一轮照旧用日期候选。
- zcode：抢领现在只在北京时间周五 18:00 到周日结束的活动窗口内跑。此前它整周每十分钟就发一次 claim，上游回 1005，攒多了直接回一个没有 body 的 `429`，本轮日志就显示成「空响应体」。窗口判断放在本轮节流标记之前，所以窗口开启后的第一个 tick 立刻就能跑；claim 遇到 `429` 也会明确写出来，不再显示成空响应体。
