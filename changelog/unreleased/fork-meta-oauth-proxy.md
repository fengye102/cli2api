### English

- meta: the device login now uses the account's `proxy_url` for all four hops — device authorization, token polling, API-key minting and every auth form post. They were built with the global egress, so an account configured to authenticate through a specific proxy actually logged in from the server's own address, which Meta sees as a different client than the one that talks to the API afterwards. An account proxy still wins over the global one, and a store failure while resolving the proxy is reported instead of silently falling back to the global egress; a not-yet-saved account row still falls back, so a fresh login keeps working.

### 中文

- meta：设备登录的四段请求（device authorization、token 轮询、铸造 API key、以及所有 auth 表单 POST）现在都走账号自己的 `proxy_url`。它们原来是用全局出口构造的，于是「配了专用代理来登录」的账号其实是从服务器本机地址登进去的，Meta 那边看到的客户端和之后调 API 的客户端不是同一个。账号代理仍优先于全局代理；解析代理时如果 store 出错会直接报出来，而不是静默退回全局出口；账号行还没落库的新登录则照旧回退，不影响首次登录。
