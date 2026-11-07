### English

- The Loomy claim message reports what actually arrived instead of the nominal task total. The eight starter tasks are advertised as 10000 points and the message printed that sum as the result, which produced "said +10000, balance looks unchanged" reports: the credits did arrive (9989 → 19987) but the points service caches reads for a few minutes and the message carried no verifiable number.
- The claim now reads available points before and after and reports both: `本次领取 8 项，任务标称 +10000 积分；可用积分 9989 → 19987（+10000）`. A claimed-already run reads `19987 → 19987（+0）；已领过 8 项`, which matches the wallet window (15000 + 4987).
- When either read fails the message falls back to the nominal total, explicitly labelled as nominal, so an unverified number never looks confirmed. Single-task claims behave the same way.

### 中文

- Loomy 的领取消息报告真实到账，不再拿任务标称总量当战果。新手任务面板 8 项标称共 10000 积分，旧消息直接打印这个和，于是出现「消息说 +10000、余额看着没动」的反馈：积分其实到了（9989 → 19987），只是积分服务读缓存几分钟，而消息里没有任何可核对的数字。
- 现在领取前后各读一次可用积分，两个数字一起给：`本次领取 8 项，任务标称 +10000 积分；可用积分 9989 → 19987（+10000）`。重复领取时读到 `19987 → 19987（+0）；已领过 8 项`，与余额窗口（15000 + 4987）对得上。
- 任一读取失败时退回只写标称值，并明确标成「任务标称」，不让未经核实的数字看起来像已确认结果。单任务领取同样处理。
