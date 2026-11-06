# 自定义中转站：把「地址 + 钥匙」当成一条账号

cli2api 的 18 个供应商，主机名都写死在 `registry.go` 的 `RegionDescriptor.ChatBase` 里。想接一个第三方中转站，只有改代码一条路。这一版补上第 19 个：**地址不在代码里，在凭证里**。

## 跟 sub2api 的对应关系

sub2api 的做法没有「新供应商」这一层：它就是一条普通账号，`platform=openai`、`type=api_key`，`credentials` 里塞 `{"api_key":…, "base_url":…}`，再配模型白名单和能力探测（`backend/internal/pkg/openai_compat/upstream_capability.go` 里写着"这些上游普遍只支持 `/v1/chat/completions`，不存在 `/v1/responses`"）。

cli2api 这边照这个思路落成 provider `custom`：

```json
{"format":"custom-relay-v1",
 "base_url":"https://某个中转站/v1",
 "api_key":"sk-...",
 "name":"备注名",
 "models":["手填的模型名"],           // 可选：上游没有 /models 时兜底
 "model_mapping":{"对外名":"上游名"}} // 可选：改名暴露
```

一条账号 = 一个中转站 + 一把 key。同一个 provider 下能挂任意多个不同地址的账号，池子按模型名路由。

## 五件事

| 环节 | 做法 |
|---|---|
| **凭证** | `base_url` 支持粘贴容错（`apiKey`/`baseUrl`/`url` 都认）；裸域名自动补 `/v1`；校验在粘贴时完成，残缺凭证直接拒，不留死账号 |
| **目录** | `GET {base}/models`，5 分钟 TTL；跳过 `type:image` 记录；`model_mapping` 的键也进目录；上游没有 `/models` 时用凭证里手填的 `models` |
| **对话** | `POST {base}/chat/completions` 透传：只改 `model`、强制 `stream`、`developer` 角色折成 `system`。**从不打 `/v1/responses`** —— 就是 sub2api 踩过的那个坑 |
| **探活** | `/models` 返回 200 即 ready；没有额度接口就不编造额度 |
| **路由** | 模型名用上游原名（客户端不用改），`custom/<模型>` 可把请求钉死在这个供应商 |

## 面板怎么加账号

两种都实测过：

1. **导入凭证**（`POST /api/accounts/import`）——贴完整 JSON，`format` 填 `custom-relay-v1`。
2. **单框粘贴**（`login/pat`）——中转站的地址和 key 拼一行即可：`https://relay/v1|sk-xxx`（竖线或空格都行）。之所以加这个，是因为面板那个粘贴框只会把整串当 `api_key` 发过来，而中转站需要两个值。

## 顺手修掉的两处

- **system prompt 被吞**：建号默认 `drop_system_prompt=true`（WorkBuddy 需要，上游会拦第三方身份）。中转站是纯透传，丢掉调用方的 system 会悄悄改变模型行为——实测 `system+developer+user` 到上游只剩 `user`。已把 `custom` 加进保留名单，并补了断言。
- **传输错误被吞导致账号冷却**：中转站 TLS 握手抖动原本会被当成"账号不可用"冷却 15 秒。凭证错误里保留 `Err` 链后按形状识别，账号继续可路由。

## 真机验证

同一 docker 网络里起一个 OpenAI 兼容的假中转站（`stub-relay:18099`），完整链路跑通：

- 导入 → `201`，探活 → `ready`
- 目录 → 47 → **49** 个模型，多出 `stub-chat` / `stub-reasoner`
- 非流式 → `200`，`usage.source=upstream`（11/7 tokens）
- 流式 `custom/stub-reasoner` → `200`，`[DONE]` 收尾
- 假中转站看到的：`Authorization: Bearer sk-stub-key`、`model` 已改成上游原名、`stream:true`、`roles:["system","system","user"]`（developer 已折叠、system 保留）
- 网关日志：`provider=custom`，状态 `ok`，tokens 与上游一致
- 未知模型 → `400 model_not_available`（不会打到上游）
- 单框粘贴建号 → `login/pat 200`，账号 `ready`
- 清理：账号删除 `204`，测试 key 与假中转站容器均已清除
