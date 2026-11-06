# Loomy：图像生成模型不再冒充聊天模型，流式请求也记积分

真机暴露两处，都是"只在一种数据形状下才出错"，所以单测一直是绿的。

## 图像生成模型出现在 /v1/models

Loomy 的模型目录里混着三个只能走 `/v1/images/generations` 的生成模型（Hy-Image-3.5-preview、doubao-seedream-5-lite、qwen-image-3.0-pro）。本网关不提供图像生成，所以 `imageModel()` 本来就要把它们丢掉——但它先判断"`modalities` 为空就当文本模型"，再去看 id，而上游恰好**根本不发 `modalities` 字段**给这三条记录。于是早退分支先把它们放行了：模型列表里能看到，一调用就是 404。

- id 判断挪到模态判断之前，并把 `image` / `seedream` / `seededit` / `seedance` 这几种命名也一并按生成模型处理，这样上游哪天再加一条同样不带 `modalities` 的生成模型也不会漏。
- 假网关改成真机的形状（生成模型不带 `modalities`）。改之前这个测试是绿的，改之后在旧代码上红、在新代码上绿——这才是能钉住问题的测试。
- 真机复核：模型总数 47 → 44，`loomy/*` 从 10 条降到 7 条，正好是那三个生成模型；其余渠道一条没少。

## 流式请求的积分是空的

同一个模型，非流式记到 1.0 积分，流式记到空。原因是流式转发只从上游 `usage` 里认 `credits` / `credit` 两个键，而 Loomy 报的是 `usage.points_consumed`——非流式走的是另一条聚合路径，那里认得这个键，所以只有流式丢。

- `ParseStreamUsageLine` 补上 `points_consumed` 作为第三顺位回退（`credits` > `credit` > `points_consumed`），不改变已有上游的取值优先级。
- 真机复核：部署后流式请求 `credits=1.0`，与非流式一致；部署前同一请求是空。
