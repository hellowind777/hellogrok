# 发布说明 — v0.1.21

## 保留 Grok Build 会话身份并兼容 OpenCode Go

OpenCode Go 现在要求请求携带 `x-opencode-session` 才能进行会话路由。Grok Build 使用自己的请求头发送稳定对话身份，因此同一个自定义渠道直接使用可能正常，但经过 hellogrok 代理后会失败。

hellogrok 现在会在内部统一识别入站对话身份，并在 OpenCode Go 渠道的上游边界投影为所需请求头：

- 优先保留已有的非空 `x-opencode-session`。
- 依次回退到 `x-grok-conv-id`、`x-grok-session-id`、`x-session-id`、`session_id` 或 `metadata.session_id`。
- 不增加配置字段，也不会为每个请求生成新的随机 UUID。
- 仅对 `opencode.ai/zen/go` 路由启用；官方 xAI、DeepSeek 及其他渠道保持不变。

升级后请重启 hellogrok；如果受影响的自定义模型在升级前已经加载，请在 Grok Build 中重新选择一次该模型。
