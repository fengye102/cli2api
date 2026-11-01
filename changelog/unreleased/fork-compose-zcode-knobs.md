### English

- deploy: the compose file now passes the zcode client-identity knobs through to the container (`ZCODE_APP_VERSION`, `ZCODE_CREDENTIAL_SECRET`, `ZCODE_CREDENTIAL_SEED`, the `ZCODE_IDENTITY_*` overrides and `ZCODE_DEVICE_MID`). Compose passes only the variables its own `environment:` block lists, so a value set in `.env` alone never reached the container — the first attempt at pinning the reported build silently changed nothing. The comment also records what a version the gateway does not know costs: `ZCODE_APP_VERSION=3.17.0`, a build ZCode never released, answered `429 code 3012 "request has been blocked due to unusual activity"` and cooled the accounts for a minute, so the knob is for rolling back to a build that shipped, not for probing.

### 中文

- deploy：compose 文件现在把 zcode 的客户端身份开关真正透进容器（`ZCODE_APP_VERSION`、`ZCODE_CREDENTIAL_SECRET`、`ZCODE_CREDENTIAL_SEED`、各 `ZCODE_IDENTITY_*` 覆盖项与 `ZCODE_DEVICE_MID`）。compose 只传它自己 `environment:` 块里列出的变量，所以以前只写进 `.env` 的值根本没进容器——第一次尝试钉版本时「看起来改了、其实没变」。注释里也记下了「版本号对不上」的代价：`ZCODE_APP_VERSION=3.17.0`（ZCode 从未发过的版本）直接换回 `429 code 3012 "request has been blocked due to unusual activity"`，并把账号冷却了一分钟。所以这个开关是用来回退到确实发布过的版本的，不是拿来试的。
