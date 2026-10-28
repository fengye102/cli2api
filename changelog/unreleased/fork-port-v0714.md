### English

- Trae now forwards `response_format`, so a client asking for `json_object` or a JSON schema gets a constrained reply instead of free text.
- New Trae and Kimi accounts keep caller system prompts by default; clients that put JSON instructions in the system prompt (MiMo Code `/goal`) no longer have them stripped. Screened families such as WorkBuddy still strip. Existing accounts keep whatever they were created with — no stored row is rewritten.
- Add Muse (Meta) as an experimental login-source provider (`meta`, alias `muse`): device-code OAuth against auth.meta.com, LLM key minting at api.meta.ai, Responses chat, and a `meta/` model prefix. Credential import recognises `meta-oauth-v1` and `meta-key-v1`.

### 中文

- Trae 现在转发 `response_format`：客户端要求 `json_object` 或 JSON Schema 时，拿到的是受约束的输出，而不是自由文本。
- 新建的 Trae 与 Kimi 账号默认保留调用方的系统提示词；把 JSON 指令放在 system prompt 的客户端（如 MiMo Code 的 `/goal`）不再被剥离。WorkBuddy 等有内容审查的渠道仍然剥离。已有账号保持创建时的取值，不改写任何存量记录。
- 新增 Muse（Meta）实验性登录源（`meta`，别名 `muse`）：走 auth.meta.com 的设备码授权，在 api.meta.ai 兑换 LLM Key，支持 Responses 聊天与 `meta/` 模型前缀。凭证导入可识别 `meta-oauth-v1` 与 `meta-key-v1`。
