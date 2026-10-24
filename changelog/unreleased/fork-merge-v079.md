### English

- Merge upstream **v0.7.9** (247 commits) into the fork. The ZCode, Raccoon and
  Antigravity channels keep working next to upstream's new QwenWork channel.
- `/v1/messages` now accepts an inline `role: "system"` message. Claude Code
  2.1.293+ emits system turns inside `messages` instead of a top-level `system`
  field, and the previous adapter rejected that shape with a 400.
- A stream that never delivers a response head now fails over to the next
  account instead of hanging, via `providers.ErrFirstByteTimeout`.
- Region-qualified model prefixes are available: `workbuddy-cn/`, `workbuddy-gl/`
  and `workbuddy-auto/`. Bare prefixes keep their previous behaviour.
- New **QwenWork** provider (`qwenwork/` prefix), configurable request-log
  retention (`CLI2API_LOG_RETENTION_DAYS`, `CLI2API_LOG_MAX_ROWS`), and the
  console Usage page gains a Cost tab.

### 中文

- 合并上游 **v0.7.9**（247 个提交）进 fork。ZCode、Raccoon、Antigravity 三个
  fork 独有渠道与上游新增的 QwenWork 渠道并存，均正常工作。
- `/v1/messages` 现在接受消息列表内联的 `role: "system"`。Claude Code
  2.1.293+ 会把 system 轮次放进 `messages` 而不是顶层 `system` 字段，旧适配器
  对这种形态直接返回 400。
- 流式请求收不到响应头时改为切换下一个账号，不再干等，由
  `providers.ErrFirstByteTimeout` 承载。
- 支持区域限定模型前缀：`workbuddy-cn/`、`workbuddy-gl/`、`workbuddy-auto/`；
  裸前缀行为不变。
- 新增 **QwenWork** 渠道（`qwenwork/` 前缀）、请求日志保留可配置
  （`CLI2API_LOG_RETENTION_DAYS`、`CLI2API_LOG_MAX_ROWS`），控制台用量页增加
  费用选项卡。
