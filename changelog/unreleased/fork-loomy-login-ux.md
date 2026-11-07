### English

- Loomy's login wizard remembers the phone number. SMS login already stored `loginid`, but the wizard only looked at the number in the request, so the operator had to retype it every time. Without a number in the request it now falls back to the 11-digit number on the credential and sends the code directly; the button reads "send code" instead of "start browser login".
- Sessions renew silently before they expire. A new `SessionRenewer` hook plus `loomy.Client.RenewSession` runs from the hourly maintenance loop: more than two days left does nothing, a credential without a stored password does nothing, and only an expiring session with a password is traded for a fresh one via `byPwd`.
- The lead time is `sessionRenewLeadSeconds * time.Second`; missing the `time.Second` made "two days" two nanoseconds, so renewal never fired. A unit test pins it.
- The Loomy wizard now has its own paste hint instead of the shared 127.0.0.1 / AutoClaw copy.

### 中文

- Loomy 的登录向导会记住手机号。短信登录成功时 `loginid` 已经存进凭证，但向导只看请求里带的号码，所以每次都要重填。现在请求里没有号码就回退到凭证里已存的 11 位手机号直接发码；按钮文案也从「开始浏览器登录」改成「发送验证码」。
- 会话到期前静默续期。新增 `SessionRenewer` 钩子与 `loomy.Client.RenewSession`，由每小时维护循环驱动：剩余超过两天什么都不做，凭证里没有明文密码也不做，只有临近过期且存了密码才用 `byPwd` 换新会话。
- 判据是 `sessionRenewLeadSeconds * time.Second`；漏乘 `time.Second` 会把「两天」当成两纳秒，等于永不续期，单测钉住了这一条。
- Loomy 向导现在有自己的粘贴提示，不再复用写死 127.0.0.1 / AutoClaw 的通用文案。
