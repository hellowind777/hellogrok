# 发布说明 — v0.1.6

## Responses 托管搜索现在返回完整来源

当 Responses 渠道使用自身的托管 `web_search` 时，hellogrok 现在会请求供应商返回完整的 `web_search_call.action.sources` 元数据。原有 `include` 项会被保留，来源请求也只会追加一次，因此供应商专用的响应字段仍可正常使用。

这些来源会进入 Grok Build 原生的去重站点数量统计，不再局限于回答正文中明确引用或实际打开的页面。

## 所有受支持协议统一归一化来源元数据

Responses 供应商可能在响应顶层返回引用，Chat Completions 中转则可能使用 `annotations`、`citations`、`search_results` 或 `web_search_results`。hellogrok 现在会在流式和非流式响应中识别这些结构，并把已验证 URL 同时转换为 `web_search_call.action.sources` 与 `output_text.annotations`。

因此，Responses、Messages 和 Chat Completions 渠道可以继续使用各自的托管搜索格式，同时向 Grok Build 输出一致的搜索调用、引用和来源数量。三种格式中的任意一种被设为 Grok Build 客户端搜索模型时，也会使用相同的来源归一化逻辑。
