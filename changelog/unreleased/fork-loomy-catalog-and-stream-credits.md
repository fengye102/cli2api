### English

- Loomy's model list no longer offers the three image-generation models (`Hy-Image-3.5-preview`, `doubao-seedream-5-lite`, `qwen-image-3.0-pro`) as chat models. The filter checked `modalities` before the model id, and the upstream omits that field for exactly those records, so the early return let them through: they appeared in `/v1/models` and answered 404 when called. The id check now comes first and also matches `image` / `seedream` / `seededit` / `seedance`.
- Streaming turns now record credits. The stream path only read `usage.credits` / `usage.credit`, while Loomy reports `usage.points_consumed`, so every streamed turn logged an empty value while the same model logged 1.0 when called without streaming.
- Live check: 47 → 44 models, `loomy/*` 10 → 7, and only the three image models disappeared.

### 中文

- Loomy 的模型列表不再把那三个只能走图像接口的模型（`Hy-Image-3.5-preview`、`doubao-seedream-5-lite`、`qwen-image-3.0-pro`）当聊天模型放出去。原来的过滤先看 `modalities`、后看模型 id，而上游恰好不给这三条记录发这个字段，于是早退分支把它们放行了：列表里看得到，一调用就是 404。现在先判 id，并把 `image` / `seedream` / `seededit` / `seedance` 一起算作生成模型。
- 流式请求现在会记积分。流式路径只认 `usage.credits` / `usage.credit`，而 Loomy 报的是 `usage.points_consumed`，所以同一个模型非流式记到 1.0、流式记到空。
- 真机复核：模型总数 47 → 44，`loomy/*` 从 10 条降到 7 条，正好少了那三个。
