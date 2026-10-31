### English

- zcode: weekend promo auto-grab. While the upstream claim window is open (Friday 18:00 through Sunday, Beijing time), the maintenance loop now tries the date-encoded weekend plan ids for every enabled zcode account with auto-checkin on, at most every 10 minutes, and stops once a promo plan is active. Refusals (1005 quota gone, 1004 no permission, 1001 unknown id) are retried next round; already-active accounts are skipped without claim calls. Ported from angusdevgo/ZCode2api.

### 中文

- zcode：周末活动包自动抢领。上游领取窗口开放期间（北京时间周五 18:00 至周日结束），维护循环会为开启自动签到的 zcode 账号每 10 分钟（上限）尝试一次按日期编码的周末包 plan id，一旦活动包生效即停止；名额已尽（1005）、无资格（1004）、id 不存在（1001）等拒绝结果留到下一轮再试，已领到的账号不再发领取请求。移植自 angusdevgo/ZCode2api。
