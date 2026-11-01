### English

- zcode: re-send a queued turn instead of cooling a healthy account. The plan gateway answers 3008/3009 ("user/egress concurrency limit exceeded") when the same user or the same egress already holds its allowed concurrent turns — three concurrent turns from one egress is enough. That was classified as a plain rate limit, so one busy moment put the whole account on cooldown and every other request for it failed fast, which reads in the console as "the channel is down". Both the streaming and non-streaming paths now sleep 2s/3s/5s and re-send the same request on the same account (up to three attempts) without touching the verification pool; only a queue that outlasts the retries surfaces, with a message that names the queue and its business code.

### 中文

- zcode：把排队中的请求重发，而不是把一个健康账号冷却掉。当同一用户或同一出口已经占满允许的并发数时，计划网关会回 3008/3009（并发上限）；同一出口三个并发就足以触发。此前它被当成普通限流，于是一个忙碌瞬间就把整个账号打进冷却，该账号的其它请求全部快速失败，控制台上看起来就像「渠道挂了」。现在流式与非流式两条路径都会按 2 秒 / 3 秒 / 5 秒退避、在同一账号上重发同一请求（最多三次），且不碰验证码池；只有超出重试次数的排队才会浮现，消息里明确写出排队原因和业务码。
