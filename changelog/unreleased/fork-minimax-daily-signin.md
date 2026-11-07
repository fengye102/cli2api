### English

- MiniMax Code now signs in for its daily reward. The account card shows 400 points a day on a seven-day cycle, and the claim used to be impossible from the console because the endpoint only exists in the bundled CLI's `/checkin` command: `GET /minimax-cloud/api/v1/signin/status` and `POST /minimax-cloud/api/v1/signin/claim`.
- Both calls take `timezone_id` as a **query parameter**. Sent as a header, or omitted, the gateway answers `1406010011 invalid timezone_id` for a token that is otherwise valid, which is what made the endpoint look unusable.
- The daily scheduler now covers MiniMax (auto check-in is off by default, like Trae). A day the panel marks claimed returns `already`, a day it has not reached returns `skipped`, and neither is treated as a failure. The message reports the balance delta read before and after the claim instead of the nominal reward.

### 中文

- MiniMax Code 现在会自动领每日签到奖励。账号卡片上写的是「每天 400 积分，七天一轮」，而签到这个动作以前在控制台做不了：接口只存在于 mcode CLI 的 `/checkin` 命令里，即 `GET /minimax-cloud/api/v1/signin/status` 与 `POST /minimax-cloud/api/v1/signin/claim`。
- 两个接口的 `timezone_id` **必须是 query 参数**。放进请求头或者不传，网关会对一个本来有效的 token 返回 `1406010011 invalid timezone_id`——这就是它之前看起来「不可用」的原因。
- 每日调度现在覆盖 MiniMax（自动签到默认关闭，跟 Trae 一样需要逐个打开）。面板显示已签到的那天返回 `already`，还没轮到的天返回 `skipped`，两者都不算失败。消息里报的是领取前后真实读到的余额差，而不是奖励面额。
