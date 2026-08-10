# 发布说明 — v0.1.5

## 每个渠道继续使用真实上游协议

hellogrok 现在通过同一个协议感知代理提供渠道隔离的 `responses`、`messages` 和 `chat/completions` 入口。普通会话继续使用渠道配置的后端。只有启用 `supports_backend_search = true` 且 Grok Build 需要 hosted 工具时，渠道才会临时向 Grok Build 显示为 Responses；hellogrok 会在供应商边界把请求和流转换回真实协议。

Responses 搜索保持原样；Messages 使用原生 hosted 搜索工具；Chat Completions 根据 `chat_search_dialect` 使用 `web_search_options`、`search_parameters`，或显式桥接到 Responses/Messages。搜索结果会转换为 Grok Build 原生的 `web_search_call`、引用和来源数量。

## 三种格式都可以作为已配置搜索模型

通过 `[models].web_search` 或 `GROK_WEB_SEARCH_MODEL` 选中的渠道，即使 `supports_backend_search` 为 false 或未配置，也可以执行 Grok Build 的客户端 `web_search`。hellogrok 会识别专用 WebSearchClient 请求，并调用该渠道真实的 Responses、Messages 或 Chat 搜索 API。未选择搜索模型时，Grok Build 继续使用已登录官方账号的默认搜索路径。

只有供应商明确证明搜索已经完成并返回非空回答时，代理才接受结果。真实来源 URL 和引用会被保留，供 Grok Build 显示原生去重站点数量；若中转忽略了搜索扩展，代理会返回上游错误，不会把普通回答误报为搜索成功。

## 跨协议转换保持工具历史有效

转发前，hellogrok 会检查三种协议各自的工具配对规则。Messages 的 `tool_use` 批次必须由紧邻的下一条 user 消息开头的 `tool_result` 完整解析；并行 Responses 函数调用在转换时会合并成一组相邻的 Messages assistant/result 消息。真正缺失的结果会返回不可重试的 `400`，不会进入供应商并触发 Grok Build 重试循环。

如果 Messages 兼容流在 thinking 块开始时漏掉必需的空 `signature`，代理会补全该字段，同时原样保留供应商后续发送的 `signature_delta`。

## 上游停滞会明确结束

hellogrok 最多等待上游响应头 180 秒，SSE 两次读取之间也最多等待 180 秒，但持续活动的流没有总时限。心跳会刷新空闲时间。响应头超时返回可重试的 `504`；流开始后发生空闲超时，则输出接收协议对应的流错误并关闭上游连接。

代理还会在规范化前记录 Responses、Messages 和 Chat Completions 原始输出声明的模型。日志可以识别中转静默替换模型或不同帧声明冲突，但不会据此改变路由或响应内容。

## 配置和渠道身份保持隔离

配置恢复现在会分别记录供应商真实协议和投影给 Grok Build 的后端，并可恢复旧版恢复事务。旧的进程内搜索重放缓存已经移除；搜索历史改为根据当前请求和已验证来源重建。

渠道自有凭据和额外请求头继续与 Grok 登录鉴权隔离。无效请求头声明或覆盖代理自有请求头的配置会在路由启用前被拒绝。
