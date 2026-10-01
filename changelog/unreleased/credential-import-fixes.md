### English

- Credential import is now deterministic: a pasted blob that carries several candidate accounts always resolves to the same one (object keys are walked in sorted order instead of Go's randomized map order, and a decoder's field-name list is honoured in the order it was written). Previously the same paste could import any of the candidates, and the mismatch was silent.
- WorkBuddy / CodeBuddy `uid+token` splitting no longer truncates tokens that contain a `+`: the part in front of the delimiter must look like a user id, so a standard-base64 token is imported intact instead of being reduced to its tail (and the uid fallback is no longer suppressed by a bogus uid).
- The WorkBuddy `{account, auth}` export shape only matches when it actually carries an access token, and an access token that lives in a sibling blob of the same dump is now found and used instead of the import failing with "requires access_token".

### 中文

- 凭证导入改为确定性行为：同一份粘贴内容里若含多个候选账号，每次导入都得到同一个结果（对象键按排序顺序遍历，不再受 Go map 随机顺序影响；解析函数的字段名按书写顺序优先）。此前同一份内容可能随机导入不同账号，且不会报错。
- WorkBuddy / CodeBuddy 的 `uid+token` 拆分不再截断含 `+` 的 token：分隔符前面必须是形如用户 id 的短标识，标准 base64 的 token 会被完整导入（也不会再因为一个假 uid 而跳过 uid 回退逻辑）。
- WorkBuddy 的 `{account, auth}` 导出结构只在确实带访问令牌时才匹配；访问令牌位于同一份数据里的其他兄弟节点时会被找到并使用，不再报 "requires access_token"。
