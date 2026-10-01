### English

- Account import now accepts raw client-side files pasted into the console: the official Codex `~/.codex/auth.json` (with `account_id` and `email` derived from token claims when absent), Trae IDE `storage.json` (including JSON-encoded auth blobs), and CodeBuddy desktop `state.vscdb` auth values (including packed `uid+token` sessions). Import parsing is shared and tolerant of nested provider layouts.

### 中文

- 账号导入现在可直接粘贴客户端原生文件内容：官方 Codex `~/.codex/auth.json`（缺失时自动从 token 声明推导 `account_id` 与 `email`）、Trae IDE 的 `storage.json`（兼容字符串化内嵌 JSON 的认证 blob）、CodeBuddy 桌面端 `state.vscdb` 认证值（兼容 `uid+token` 打包格式）。导入解析逻辑已统一为可容错嵌套结构的公共实现。
