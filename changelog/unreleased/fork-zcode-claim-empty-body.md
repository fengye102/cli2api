### English

- zcode: an empty 2xx body on `/billing/claim` no longer reads as a claim. An empty body unmarshals to `code: 0`, which is also the success code, so a gateway hiccup could make the weekend sweep report a promo it never received and stop for the whole weekend. A success now has to name the plan (or say something), and a refusal names its status instead of leaving the sweep's log line ending in a bare colon.

### 中文

- zcode：`/billing/claim` 返回 2xx 但空响应体时不再算作领取成功。空体反序列化后 `code` 为 0，而 0 恰好也是成功码，于是网关一个抖动就可能让周末抢领误报「已领到」并整个周末不再重试。现在成功必须能指出具体 plan（或至少带 msg），而拒绝会写明状态码，不再让抢领日志以冒号结尾。
