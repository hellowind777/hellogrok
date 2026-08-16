# 发布说明 — v0.1.10

## 上下文统计与自动压缩遵循 Grok Build 原生合同

所有渠道只要返回完整可信的输入/输出计数，hellogrok 都会提供 `usage.context_details`，并兼容常见的 `prompt_tokens` / `completion_tokens` 及详情字段别名。缺失、冲突、小数、溢出或占位用量保持为 null，让 Grok Build 保留原有实时上下文基线，不再被重置为零。

容量发现由配置和远端元数据驱动，不按模型 ID 写死。显式模型或 provider 上限始终优先；否则可采用有效的上游 `X-Grok-Context-Window`、`X-Grok-Max-Completion-Tokens`，或唯一且无歧义的结构化上下文上限字段，最后才回退到 Grok Build 模型目录。已经超过阈值的恢复会话因此能在下一次用户输入前立即压缩，Responses、Messages 与 Chat Completions 均已覆盖。

## 输出预留不再被误判为更小的上下文窗口

DeepSeek 文档中的 1M 上下文包含输入和生成输出，384K 输出上限同时包含推理与可见正文。当供应商明确证明请求只是因为 Grok Build 预留了完整输出额度而失败时，hellogrok 会把本次请求缩小到精确剩余空间并只重试一次。只有数字自洽的 HTTP 400 `invalid_request_error` 才能触发；歧义、冲突、已耗尽、格式错误或重复失败都会原样返回。

Responses 的 `status = "incomplete"` 与 Chat 的 `finish_reason = "length"` 会保留供应商已生成的部分结果，并统一标记为 `max_output_tokens`。单次输出额度耗尽与活动总上下文耗尽保持为两个不同概念。

## 推理配置保持紧凑并尊重用户设置

hellogrok 生成的 DeepSeek 菜单现在只有一行原生配置：

```toml
reasoning_efforts = ["none", "low", "high", "max"]
```

hellogrok 不再写入冗余的 `supports_reasoning_effort` 或四段重复对象表。已有用户列表、空列表、自定义标签、顺序和默认值均不会被替换或追加。只有旧版 hellogrok 精确生成的冗长菜单会迁移一次，并保留原来的 High 默认档位。

## 第三方渠道保留 Grok Build 本地工具

Responses、Messages 与 Chat Completions 桥接会保留 Grok Build 任意函数工具及其历史，包括 Shell、文件、补丁、Task 和 MCP 函数。第三方请求会一致移除 xAI 专属 `x_search`，但不会删除普通函数或供应商 `web_search`。

DeepSeek 官方 Responses 与 Messages hosted 搜索请求现在保持配置的原生 API。DeepSeek Chat 默认使用供应商已正式支持的 Responses API，显式 `chat_search_dialect = "messages"` 仍然有效。
