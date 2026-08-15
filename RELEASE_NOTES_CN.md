# 发布说明 — v0.1.9

## DeepSeek Web Search 默认保留 Grok Build 来源详情

当 `api.deepseek.com` 官方请求可以执行 hosted 搜索且没有配置搜索协议覆盖时，hellogrok 现在会让该搜索轮次使用官方 Anthropic Messages API。DeepSeek Messages 会返回结构化 `web_search_tool_result` URL，Grok Build 因而可以原生显示站点数量、来源域名和可展开的编号链接。

这一协议选择只在当前请求确实允许 hosted 搜索时生效。若 `tool_choice` 禁用搜索或指定其他函数，请求仍使用渠道配置的 Responses、Messages 或 Chat Completions API。显式设置的 `chat_search_dialect = "responses"` 或 `"messages"` 始终优先，并可从任意已配置后端桥接。

## Responses 来源发现面向未来兼容

每个 Responses hosted 搜索请求现在都会请求 `web_search_call.action.sources`，同时保留调用方已有的 `include` 项目且不重复追加。DeepSeek 当前会接受但静默忽略该提示；保留这一标准字段后，供应商未来实现支持时，来源 URL 会自动进入现有归一化链路。hellogrok 不会伪造上游省略的来源。

DeepSeek Responses 返回的 `action.queries` 会映射到 Grok Build 用于显示的单值 `action.query`，原始数组仍保留用于后续回传。Messages 搜索结果还会保留 `title`、`page_age`、`encrypted_content` 等供应商元数据；已有权威来源时，不再混入只有引用语义的 URL。

## 客户端搜索能够返回最终文本

Grok Build WebSearchClient 适配器不再把自动工具选择改成永久强制的服务端搜索工具。具备 Agent 行为的供应商可以先完成 Web Search，再返回 Grok Build 所需的最终文本，避免重复搜索直到轮次结束却没有可用输出。

## 桥接搜索历史保持缓存稳定

Responses 转 Messages 时，历史 backend Web Search 调用现在使用与 Grok Build 相同的稳定可读摘要，不再重建并非由 Messages API 返回的虚假供应商服务端工具块。实时调用和结果仍使用供应商原生格式；Responses、Messages 与 Chat Completions 的原生及桥接缓存前缀继续由矩阵测试覆盖。
