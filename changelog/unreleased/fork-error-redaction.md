### English

- gateway: redact credentials in upstream error bodies at the one boundary every provider shares. Only CodeArts stripped tokens from the message it surfaced; every other provider's error body went into `request_logs.error_message`, the `/api/logs` console and the caller's HTTP response verbatim, so an upstream that echoed the rejected `Authorization`, `x-auth-token`, Huawei `Access=`/`Signature=` pair, a `sk-…` key or a bare JWT leaked it. `providers.RedactSecrets` now runs inside the executor's error path before the 500-byte cut, so one choke point covers all providers, including ones added later, and truncation can no longer leave half a secret behind.

### 中文

- gateway：在所有 provider 共用的那个出口上脱敏上游错误体里的凭据。此前只有 CodeArts 会把自己回显的消息里的 token 抹掉，其余 provider 的错误体会原样进入 `request_logs.error_message`、`/api/logs` 控制台和调用方的 HTTP 响应——上游若回显被拒的 `Authorization`、`x-auth-token`、华为 `Access=`/`Signature=`、`sk-…` 密钥或裸 JWT，就会直接泄露。现在 `providers.RedactSecrets` 在 executor 的错误出口处、500 字节截断之前执行，一个收口点覆盖全部 provider（包括以后新加的），且截断不会再留下半个密钥。
