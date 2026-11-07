### English

- Loomy's three image models are usable again through `POST /v1/images/generations`. They were correctly removed from the chat catalog (the upstream answers 404 "model not open yet" on `/v1/chat/completions`) but they do work on their own route: OpenAI-shaped request and response, `points_consumed` in the reply, a signed URL valid for 12 hours, 110 credits per image on all three.
- New optional `ImageGenerator` capability: catalog entries carry `type:"image"` and `supports_image_generation`, so a client can tell "can make images" from "can read images". Sending a chat request to one of these models is rejected with a pointer to the image endpoint instead of a wasted upstream round trip.
- Live verification produced real 1024×1024 PNGs from `Hy-Image-3.5-preview` and `doubao-seedream-5-lite`.

### 中文

- Loomy 的三个图像模型重新可用，走 `POST /v1/images/generations`。上一版把它们从聊天目录里清掉是对的（上游在 `/v1/chat/completions` 上直接 404「该模型暂未开放」），但它们在自己的路由上是通的：入参出参都是 OpenAI 形状，响应里带 `points_consumed`，URL 有 12 小时签名，三个模型实测都是 110 积分一张。
- 新增可选的 `ImageGenerator` 能力：模型条目带 `type:"image"` 和 `supports_image_generation`，客户端因此能区分「能生成图」和「能看图」。拿这些模型发聊天请求会被直接拦下并指路图像接口，不再白跑一趟上游。
- 真机复核用 `Hy-Image-3.5-preview` 和 `doubao-seedream-5-lite` 各生成了一张真实 1024×1024 PNG。
