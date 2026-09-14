# 发布说明 — v0.1.34

## 可重试的 Responses `response.failed` 事件现在会被吸收，而不是直接透传

- **你看到的现象。** 中转在限流或并发压力下返回的 Responses SSE，其唯一终态是可重试的 `response.failed`（`rate_limit_exceeded`、`Concurrency limit exceeded`、过载）。此前 hellogrok 会立即把该失败透传给 Grok Build，本轮直接失败——而几秒后重试本可成功；重试还会为同一请求再开一条流。
- **本次变更。** 在尚未向客户端写入任何内容时，这类“只有失败终态”的 SSE 现在进入吸收窗口：响应头和早期帧先缓冲（最多 32 帧），并按与 HTTP 软故障相同的指数退避与 `Retry-After` 规则在代理内重放。Grok Build 的重试预算分毫不动；只有窗口耗尽后才以可重试形式透传。`absorb_retry_max_secs = 0` 则直接透传该失败事件。确定性 `response.failed`（鉴权、无效请求或模型）仍直接透传，不重试。
- **重试分类放宽。** 并发限制拒绝（`concurrency`、`concurrency limit` 及对应中文“并发限制/并发超限”）现在归为瞬态故障；嵌套在 `response.error` 下的错误信封与顶层 `error` 同等识别。

## 客户端断开不再被记为流错误

- **你看到的现象。** 在流式传输中途关闭 Grok Build 会话，日志或迟到的读取者可能看到 `proxy_stream_error`，像是上游出了故障，而实际只是客户端已离开。
- **本次变更。** Messages、Chat Completions、原生与 Responses 流现在区分客户端中止（客户端写入失败、请求上下文取消）与上游故障：中止只记录为 `aborted by client`，不产生流错误。上游真正截断的流仍会产生 `proxy_stream_error`。

## 流形态不匹配现在明确报错

- 非流式请求收到 SSE 响应，或 Grok Build 固定的非流式 WebSearchClient 请求收到流式响应时，返回不可重试的 `502` 并说明不匹配原因，不再转发无法解码的正文。

## 更安静、更安全的诊断日志

- 上游 HTTP 错误与 Responses `response.failed`/`error` 事件按结构化摘要（`type`、`code`、`message`）记录；bearer 令牌、key 赋值和 `sk-` 值会被脱敏，超长消息会被截断。
- 良性上游模型不一致按渠道/协议/配置模型/上游模型组合只记录一次；冲突与无效声明每次都记录。
- 当 Grok Build 在非 xAI 自定义渠道上发送 `grok-4.6` 这类官方目录名时，代理会记录一条警告（含请求体大小、工具数量和会话状态），便于诊断 `/resume` 选路。官方 `api.x.ai` 路由与 `grok4.6-sevnx` 这类自定义 ID 不在警告范围内。

升级后请重启两个 hellogrok 可执行文件。
