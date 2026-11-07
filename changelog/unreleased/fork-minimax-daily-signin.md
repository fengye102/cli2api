# MiniMax Code 每日签到

MiniMax Code 每天发 400 积分，七天一轮（第 4、7 天各 1000），积分 30 天有效。
以前控制台只能看到已经发到账的积分包，签到这个动作没法做，因为入口不在网页端。

## 端点

签到在 mcode CLI 的 `/checkin` 命令里，两个接口都在 agent 域名下：

```
GET  /minimax-cloud/api/v1/signin/status   七天线面板（每天 status: 1 未到/2 可签/3 已签）
POST /minimax-cloud/api/v1/signin/claim    请求体 {}，返回 claim_result 1 已签 / 2 重复签
```

**`timezone_id` 必须是 query 参数。** 放进请求头、或者不传，网关会对一个完全有效的
token 返回 `1406010011 invalid timezone_id` —— 这正是之前这个接口看起来"不可用"的原因。

## 行为

- 面板里有 `is_today` 的那一天就是今天：`status=2` 才去 claim，`status=3` 直接返回
  `already`，还没轮到的天返回 `skipped`。三种都不是错误，调度器每天调一次。
- `claim_result=2`（面板还没刷新但已经签过）也按 `already` 处理，不当失败。
- 消息里报的是**前后余额差**（`余额 0 → 400`），不是奖励面额；余额读不到时退化成
  面额，不影响已经完成的领取。
- 时区默认 `Asia/Shanghai`，可用 `CLI2API_MINIMAX_CHECKIN_TZ` 覆盖（非法值回落默认）。

## 接线

- `internal/providers/minimax/checkin.go`：实现 `providers.AccountCheckiner`。
- `internal/providers/registry.go`：minimax 两个区域都声明 `Checkin` 策略，控制台据此
  显示签到按钮和自动签到开关。
- 账号默认 `auto_checkin=false`，和 trae 一样需要逐个打开。
