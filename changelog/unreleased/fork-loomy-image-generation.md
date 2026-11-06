# Loomy 的三个图像模型能用了：/v1/images/generations

上一版把 Hy-Image-3.5-preview、doubao-seedream-5-lite、qwen-image-3.0-pro 当垃圾清出了模型列表——清对了（它们确实不能在 `/v1/chat/completions` 上用，上游直接 404「该模型暂未开放」），但也确实还能用：网关给它们留了另一条路。

## 上游到底是什么

`GET {modelBase}/models` 里这三条记录带着 `"type":"image"` 和 `"capabilities":{"output_modalities":["image"]}`。真正的入口是：

```
POST {modelBase}/images/generations
→ {"model":"Hy-Image-3.5-preview","prompt":"a red apple","n":1,"size":"1024x1024"}
← {"created":1793948356,
   "data":[{"url":"https://aigc-output-image-….png?q-signature=…"}],
   "points_consumed":110}
```

入参出参都是 OpenAI 形状，`points_consumed` 是这次请求实际扣的积分（账单里 `pointsActual` 对得上，实测三个模型都是 **110 积分/张**）。URL 带 12 小时签名，所以原样透传、不落地转存。

## 改了什么

- **providers**：新增可选的 `ImageGenerator` 能力，模型条目带 `supports_image_generation` 和 `type:"image"`，这样客户端能区分「能生成图」和「能看图」。
- **loomy 目录**：这三条不再被丢掉，而是带着 `image_generation` 标记（同时摘掉 `tools`）留在 `/v1/models` 里；拿它们发聊天请求会被直接拦下并指路 `/v1/images/generations`，不再白跑一趟上游换 404。
- **loomy 图像**：转发时把 `model` 改写成上游原生名，其余字段（size/n/seed…）原样透传；上游错误走统一分类，成功时返回 `points_consumed` 作为积分。
- **executor**：`GenerateImage` 复用了聊天的路由循环——密钥授权、账号 pin、冷却、并发槽、每次尝试的审计记录都一样，分类错误照样换号重试。
- **gateway**：`POST /v1/images/generations`，与聊天一样落请求日志（含积分）。

## 顺手修掉的一个真问题

第一次真机跑图像时，一次瞬时 TLS 握手超时把唯一的 loomy 账号判成 unavailable 冷却 15 秒，紧接着下一张图直接 429「all accounts are cooling down」。根因不在图像代码：`providers.Error` 只带一段字符串消息，把底层 `*url.Error` 丢了，于是执行器认不出「这是网络问题、跟账号无关」，套用了通用冷却。修法是让 `providers.Error` 带上 `Err` 并 `Unwrap()`，loomy 的聊天和图像通道都保留错误链；传输失败按形状识别，账号继续可路由。单测钉住：去掉 `Unwrap` 就是 `Cooldown:15s Failover:true`，加上就是不冷却。

## 真机验证

| 模型 | 结果 | 耗时 | 扣分 |
|---|---|---|---|
| loomy/Hy-Image-3.5-preview | 200，URL 可下载，1024×1024 真 PNG | 15.7s | 110 |
| loomy/doubao-seedream-5-lite | 200，同上 | 12.8s | 110 |
| loomy/qwen-image-3.0-pro | 200，同上 | 10.5s | 110 |

`/v1/models` 共 47 条，其中 3 条 `type=image`。日志里图像请求 `credits=110`、账号 `ready` 无冷却；拿 `loomy/qwen-image-3.0-pro` 发聊天请求返回 400 并指向图像端点。

## 代价

110 积分/张，账号现有 ~19.3k，约 170 张。图像模型不能走 `/v1/chat/completions`——Hermes 这类只调聊天接口的客户端用不了，要用 curl 或任何支持图像端点的 OpenAI SDK。
