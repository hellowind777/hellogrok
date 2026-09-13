# 发布说明 — v0.1.32

## DeepSeek 下线 hosted 搜索后，其模型不再获得隐式后端搜索默认值

- **旧默认值现在会把 `web_search` 引向死路。** 此前 hellogrok 会对精确指向官方 `api.deepseek.com` 端点且未配置该字段的模型，临时投影为 `supports_backend_search = true`——依据是 DeepSeek 文档承诺的供应商托管搜索。DeepSeek 已下线该能力：当前 Responses API 将 `web_search` 及全部内建工具类型标记为忽略，响应格式也不再包含 `web_search_call`。在旧默认值下，`web_search` 调用被路由到 DeepSeek 渠道，端点不执行任何搜索就静默作答，Grok Build 显示一个没有任何来源的"已完成"回合——而客户端搜索回退链（`[models].web_search`、`GROK_WEB_SEARCH_MODEL`、已登录官方账号回退）被整体绕过，因为该渠道看起来具备搜索能力。

- **字段缺省时的行为现在与其他供应商完全一致。** 隐式默认值已移除。缺省一律保留 Grok Build 模型目录行为：客户端 `web_search` 依次从 `[models].web_search`、`GROK_WEB_SEARCH_MODEL` 或已登录官方账号回退中解析；全都不可用时，模型如实报告无法使用 web 搜索，而不是假装搜索。hellogrok 不再为缺省该字段的 DeepSeek 渠道向活动配置写入 `supports_backend_search = true`。显式值的含义不变：`true` 仍作为路由声明被尊重（对仍实现真实搜索扩展的中转端点有用；官方端点会静默忽略），`false` 仍保持关闭，除非该渠道被选为默认搜索模型。

- **其他 DeepSeek 行为均未改变。** `[1m]` Anthropic Messages 别名、思考模式规范化、覆盖官方十分钟排队的 660 秒空闲策略、Bearer/`X-Api-Key` 鉴权，以及 Chat 到 Responses 的搜索桥接方言全部保持原样。升级前已显式设置 `supports_backend_search` 的配置行为不变。

文档同步反映：DeepSeek 2026-09-10 公告以模型 ID `deepseek-flash` 上线 V4.1 Flash 一代（已退役的 `deepseek-v4-flash` / `deepseek-v4-flash-vision-exp` 名称仍路由到该模型），V4 Pro 服务继续提供。既有 DeepSeek 渠道配置无需任何改动。

升级后请重启两个 hellogrok 可执行文件。
