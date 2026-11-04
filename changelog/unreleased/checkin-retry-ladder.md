### English

- A failed daily check-in is now retried on a backoff ladder (15m, 30m, 1h, 2h, 3h) instead of a single retry at 21:00. Upstream refusals are usually transient — trae answers `9074 当前参与用户太多，请稍后再试`, raccoon answers `今日未见积分入账（发放触发未生效，稍后重试）` — so one retry hours later often still hit a busy window and cost the day's free credits. Observed: 4 of 9 trae accounts and 1 of 8 raccoon accounts lost their credits today.
- Retries stop at 23:40 local, cap at 12 attempts per day, and keep the old 21:00 evening slot as a final catch-up once the ladder is exhausted.
- Answers that cannot change before tomorrow (`已签到 / 已领取 / not supported / 无签到`) are not retried, so a provider without a check-in API is no longer polled all day.

### 中文

- 每日签到失败后改为按阶梯重试（15 分钟、30 分钟、1 小时、2 小时、3 小时），不再只是 21:00 补一次。上游拒绝大多是临时的——trae 回 `9074 当前参与用户太多，请稍后再试`，raccoon 回 `今日未见积分入账（发放触发未生效，稍后重试）`——原来几小时后只补一次，经常又撞上繁忙窗口，当天免费额度就此丢掉。实测今天 trae 9 个号丢 4 个、raccoon 8 个号丢 1 个。
- 重试最晚到本地 23:40，每天最多 12 次，阶梯走完后仍保留原来的 21:00 晚间补签作为最后兜底。
- 当天不可能改变的答复（`已签到 / 已领取 / 不支持 / 无签到`）不再重试，没有签到接口的渠道不会被整天轮询。
