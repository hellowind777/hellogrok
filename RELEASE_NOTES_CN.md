# 发布说明 — v0.1.30

## hosted 搜索改为协商封闭 Responses schema，不再假设供应商容忍

- **标准来源提示保持默认，封闭枚举用证据判定而非猜测。** 每个 Responses hosted 搜索请求仍会请求 `web_search_call.action.sources` —— 这是 OpenAI 文档化的 `include` 值，承载 Grok Build 原生站点数背后的完整被查阅 URL 列表。实现或忽略该提示的供应商保持最大保真度。`include` 为封闭枚举的供应商不再打断搜索：hellogrok 现在依据证据协商偏差，而不是依赖供应商白名单。

- **已知封闭 schema 主机提前处理。** 火山方舟（`*.volces.com`）会以 `InvalidParameter` 拒收未知 `include` 值，并拒收携带封闭 tool schema 之外字段的 hosted 搜索声明（例如 Build 的 `filters` 域名过滤、`allowed_tools` 选择器形态）。对这些主机，hellogrok 省略该提示并发送净化后的声明：tool 内未知字段被剥离到文档集合（`type`、`max_keyword`、`limit`、`sources`、`user_location`），纯 hosted 搜索的 `allowed_tools` 选择折叠为等价的 `required` 选择器。此类主机的来源展示回落到响应注解中的 URL，hellogrok 本就将其归一化到 Grok Build 的两条展示路径。

- **未知严格供应商一次重放自愈，不保留任何状态。** 其他任何以 `400` 自证封闭 schema 的上游（拒收 include 或拒收 tool 声明）会触发同一请求内的单次重写重放：剥离 include 列表，或净化声明，然后重发同一请求。重写按请求单次执行，不会循环；被拒请求未执行搜索，重放无副作用、不消耗计费搜索额度。不保留任何按主机、按渠道或落盘的记忆：协商是线路交换的属性，因此中转、新主机与未来供应商无需 hellogrok 更新即可收敛。

- **省略解析器必需索引的引用条目不再导致整个响应失败。** Grok Build 的注解解析器要求 `url_citation` 条目带 `start_index`/`end_index`，而部分 Responses 供应商只返回 `url`、`title` 与元数据。缺失索引现在在全路由补 `0`，覆盖非流式响应体与流式终止事件。代价是引用高亮精度；否则代价是整个搜索响应被拒。

## 对真实封闭 schema 供应商的端到端验证

该协商机制已用火山方舟豆包搜索作为配置的 Grok Build `web_search` 后端实测：直连 API 以 `400 InvalidParameter` 拒收该提示与未净化声明，而协商后的请求返回 `200`，带已完成的 `web_search_call` 与注解来源（代理日志 `search evidence declared=true calls=1 completed=1 sources=5 annotations=5`）。新增测试固定每种行为：include 被拒时不带提示重放一次、tool 声明被拒时带净化声明重放一次、索引补齐适用于全路由、合规中转的 include 条目与选择器保持原样。

所有变更都留在既有安全契约内：重写发生在 Grok Build 严格校验器看到线路之前，确定性拒绝仍不可重试，且未新增任何配置面 —— 协商自动进行，除非记录了 schema 偏差否则不可见。

升级后请重启两个 hellogrok 可执行文件。
