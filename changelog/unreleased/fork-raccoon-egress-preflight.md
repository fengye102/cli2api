### English

- raccoon: a round with no proxy no longer hands out an authorize URL that cannot finish. On this deployment the site's WAF drops TCP from our own egress address - ICMP still answers and the identical request over the socks egress the accounts carry returns 200 at once - so the code exchange used to sit on a thirty-second timeout and fail with "context deadline exceeded", which reads like a hang rather than a route problem. A round with no proxy now pre-flights the dial (four seconds) before the operator touches a browser, and the paste re-checks it.
- The refusal says what was measured instead of inventing a site policy. It names the source-address block, the host and port, and both settings that fix it, and it quotes the egress this deployment already uses when one is configured. It does **not** claim the site refuses direct connections: it does not, another egress reaches it, and an operator who knows that would rightly stop trusting the check.
- The pre-flight is advice, not a gate: `skip_egress_probe` (or its alias `force`) on the login request waves it away for that call, for an operator who has measured the route themselves or is waiting out a block that may lift. Proxied rounds are untouched, successes are cached for two minutes while failures never are, and loopback or private auth bases - test servers, local relays - are exempt.

### 中文

- raccoon：没配代理的轮次不再发出一个注定完不成的授权链接。在这台部署上，站点的 WAF 会丢弃我们自家出口地址的 TCP——ICMP 照样回，同一请求走账号上那条 socks 出口立刻 200——所以换凭证以前要卡满三十秒、最后报「context deadline exceeded」，看起来像卡死，其实是路不通。现在没配代理的轮次会在操作者打开浏览器之前先做一次拨号探测（四秒），粘贴时再复查一次。
- 拒绝时说清「测到了什么」，而不是编一条站点政策。它会点明被拦的出口 IP、目标主机与端口、两个能修的设置项，并在本部署已配过出口时直接写出那个出口。它**不会**声称「这个站不接受直连」——事实不是这样，换出口就通，知道这一点的操作者会理所当然地不再相信这个检查。
- 探测只是提示，不是闸门：登录请求带上 `skip_egress_probe`（或别名 `force`）即可对当次调用跳过，适用于自己测过链路、或在等封禁自然解除的操作者。已配代理的轮次不受影响；探测成功缓存两分钟、失败永不缓存；回环或内网 auth base（测试服务器、本地中继）豁免。
