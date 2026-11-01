### English

- zcode: every upstream request now goes over HTTP/1.1, as every real ZCode client does. The Go default transport offers `h2` in the TLS ALPN extension wherever a server suggests it, and `zcode.z.ai` does; ZCode itself (Node's fetch), its OpenCode plugin (Bun's fetch) and zcode2api-plus (Go, h2 off) all speak HTTP/1.1, which changes what the TLS hello shows Alibaba Cloud's firewall. The same Start Plan request — same path, headers, auth and body — has been observed answering `405` / code 3012 network-firewall-blocked over h2 while a plugin run of the same account on the same machine got through over h1. A turn is a single stream here, so there is no multiplexing to lose.

### 中文

- zcode：所有上游请求改走 HTTP/1.1，与真正的 ZCode 客户端一致。Go 默认 transport 只要服务器在 TLS ALPN 里给了 `h2` 就会用，而 `zcode.z.ai` 就给了；但 ZCode 本体（Node fetch）、它的 OpenCode 插件（Bun fetch）、zcode2api-plus（Go，关掉 h2）清一色是 HTTP/1.1——这直接改变了 TLS hello 里给阿里云防火墙看到的东西。同一个 Start Plan 请求（路径、头部、鉴权、请求体都一样）在 h2 下出现过 `405` / code 3012「network firewall blocked」，而同机同时刻用插件跑同一账号却能通。这里的每一轮对话本来就是一条流，放弃 h2 不损失多路复用。
