### English

- zcode: tell an Aliyun edge block page apart from an API answer. When the egress is blocked, the edge answers 403/405/502 with an HTML (or XML) page rather than a JSON envelope; reading the status first turned that into an auth failure, so a healthy credential was marked "login failed" and the operator deleted a working account instead of changing its proxy. The classifier now recognises edge/WAF markers and markup bodies ahead of the status branch, reports them as unavailable, and names the fix in the message ("change the account's proxy or retry later") with a one-line body excerpt. A 3012 risk block wrapped in a page still takes the captcha path.

### 中文

- zcode：把阿里云边缘封锁页和正常 API 回应区分开。出口被封时，边缘会用 403/405/502 返回一张 HTML（或 XML）页面而不是 JSON 信封；此前先看状态码会把它当成鉴权失败，于是健康的凭据被标成「登录失败」，运维去删一个本来可用的账号，而不是换出口。现在分类器在状态码分支之前就识别边缘/WAF 标记与 markup 响应体，归类为不可用，并在消息里直接写出处置办法（「换该账号的代理或稍后重试」），同时附一行响应体摘要。若页面里包的是 3012 风控，仍走验证码路径。
