### English

- New `custom` provider: a third-party relay is now a normal account whose base URL and key live in the credential instead of in `registry.go`, matching how sub2api treats one (`platform=openai`, `type=api_key`). The console form takes the relay address and its key; import also accepts a `custom-relay-v1` bundle and a `url|key` line.
- Relays are assumed to serve only `/v1/chat/completions`. Models are discovered from `GET /models` (five-minute cache), image models are filtered out of the chat list, `model_mapping` aliases work, and `/v1/responses` is never used.
- The custom provider keeps system prompts instead of dropping them, so a relay account behaves like a first-class provider rather than a stripped proxy.

### 中文

- 新增 `custom` 供应商：第三方中转站现在是一条普通账号，地址和 Key 存在凭证里而不是写死在 `registry.go`——跟 sub2api 的做法一致（`platform=openai`、`type=api_key`）。控制台表单直接填中转站地址和 Key，导入也支持 `custom-relay-v1` 凭证包和 `url|key` 一行式。
- 中转站默认只当它支持 `/v1/chat/completions`。模型从 `GET /models` 拉（缓存五分钟），画图模型从聊天列表里过滤掉，`model_mapping` 别名可用，绝不调用 `/v1/responses`。
- `custom` 保留系统提示词而不是丢弃，所以中转站账号的表现跟原生供应商一致，而不是被削过的代理。
