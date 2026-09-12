# 发布说明 — v0.1.24

## 原生 Chat 对齐 Grok Build

- 普通 Chat / Messages 渠道继续走 Grok Build 的第一方映射器。Responses 投影只用于托管搜索和 WebSearchClient。
- 原生 Chat SSE 在 last-write-wins 之前重组工具调用：后续空 `"name"` 不再抹掉已知工具，不完整的参数 JSON 不再按帧改写。
- 空的或厂商私有的 `finish_reason` 会删除或映射为 `stop` / `length` / `tool_calls` / `content_filter` / `function_call`。
- 思考只作为前缀兄弟。答案后的 CoT、正文里的 `<think>`，以及 `reply only:` / `任务已全部完成` 这类协议自语会剥掉。推理增量保留 BPE 词首空格。
- Chat 历史保留同轮工具循环的 `reasoning_content`，跨轮明文 CoT 会剥掉（DeepSeek / MiMo 除外），不注入思考占位符。加密或带签名的块原样通过。
- 发给上游的工具表只留 Grok Build 的 `client_name`。Claude/Codex 别名只在回来的调用上改写。
- 每个上游请求都按 `grok-shell` 出示。流式 Chat 保留 `include_usage`；带工具的 GLM Chat 在缺省时设置 `tool_stream=true`。

升级后请重启 hellogrok 并新开 session。已存盘的气泡不会改写。未在当前请求中声明的工具不会被凭空创造。
