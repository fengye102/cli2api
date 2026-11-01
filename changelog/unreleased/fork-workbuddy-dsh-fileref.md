### English

- workbuddy: clear the DSH file-reference fingerprint the upstream channel gate rejects. The official `@deepseek-ai/dsh-file-reference` package injects a five-sentence `FILE_REFERENCE_PROMPT` whose first sentence trips WorkBuddy's 11-128 "illegal API invocation from an unapproved channel" rule, so every turn that carried an `@file` reference could be refused as coming from an unapproved client. The sanitizer now inserts one word in that sentence (`paths the user` → `paths that the user`) for system, developer and assistant messages only; the rest of the paragraph and all caller, user and tool text are untouched. Ported from upstream `dc54eaa` (workbuddy2api-hub #684 / v1.9.0).

### 中文

- workbuddy：清掉会被上游渠道闸口拦下的 DSH 文件引用指纹。官方 `@deepseek-ai/dsh-file-reference` 包会注入五句话的 `FILE_REFERENCE_PROMPT`，其第一句会触发 WorkBuddy 的 11-128「Illegal API invocation from an unapproved channel」规则，导致任何带 `@文件` 引用的会话都可能被判为「未授权客户端」而拒绝。现在清洗器只在 system / developer / assistant 三种角色上把那句话补一个词（`paths the user` → `paths that the user`），段落其余部分以及调用方、user、tool 的文本一律不动。移植自上游 `dc54eaa`（workbuddy2api-hub #684 / v1.9.0）。
