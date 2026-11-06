### English

- Inline base64 images are now shrunk before dispatch: anything longer than 1568px on its longest side or heavier than 700KB is downscaled and re-encoded once, in the gateway, instead of being forwarded at screenshot resolution. A single 2400x1600 PNG is ~2.7MB of base64 that vision models throw away by resizing internally — so this cuts request size and vision tokens without losing detail the model would have kept.
- Opaque images become JPEG (quality 85, sharp enough for screenshots and code), images with transparency stay PNG, and the loop keeps tightening until the encoded size fits. Re-encodes that would grow the payload are discarded, so an already-optimised upload is left byte-identical.
- Remote `http(s)` images and malformed base64 are passed through untouched; nothing is fetched.
- Tuning: `CLI2API_IMAGE_MAX_EDGE` (default 1568), `CLI2API_IMAGE_MAX_BYTES` (default 716800), `CLI2API_IMAGE_COMPRESS=off` to disable.

### 中文

- 内联 base64 图片在转发前会先缩一次：最长边超过 1568px 或超过 700KB 的图，在网关里缩好、重编码后再发给上游，不再按截图原尺寸转发。一张 2400x1600 的 PNG 大约 2.7MB 的 base64，而视觉模型内部还会把它缩小——所以这一步省下的是请求体积和视觉 token，不损失模型本来会保留的细节。
- 不透明的图转成 JPEG（质量 85，截图和代码都够清晰），带透明的图保持 PNG；循环会持续收紧到编码后体积达标。如果重编码反而变大，就丢弃重编码结果——已经优化过的图保持原样，一个字节都不动。
- 远程 `http(s)` 图片和非法 base64 一律原样放过，不会去抓取。
- 可调：`CLI2API_IMAGE_MAX_EDGE`（默认 1568）、`CLI2API_IMAGE_MAX_BYTES`（默认 716800）、`CLI2API_IMAGE_COMPRESS=off` 关闭。
