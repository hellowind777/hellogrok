# 发布说明 — v0.1.19

## hosted search 的累计计费量不再误触发自动压缩

Grok Build 把 Responses 终止包里的 `usage` 当作当前活动上下文。DeepSeek hosted Web Search 这类供应商可能返回多步累计计费 token，而不是最终 prompt 大小。这个数字一旦超过模型的 `context_window`（例如 `1.7M / 1.0M`），Grok 会立刻压缩，即使真实对话仍在窗口内。

hellogrok 现在会丢弃大于已知 `context_window` 的输入或输出计数，改为转发 `usage: null`，让 Grok 保留上一跳基线。窗口内的用量，以及自身也落在窗口内的供应商 `context_details`，保持不变。检查优先使用模型配置的窗口；未配置时才采用可信的上游 `X-Grok-Context-Window` 响应头。

升级后请重启代理。自定义模型请继续显式填写 `context_window`，否则这条护栏不会生效。丢弃事件记为 `usage discarded: live context exceeds window`。
