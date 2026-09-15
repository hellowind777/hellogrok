# 发布说明 — v0.1.35

## 丢失首个流式增量的工具调用现在会被修复，而不是报错

- **你看到的现象。** 部分第三方中继会间歇性丢掉工具调用的第一个流式增量——携带 `function.name` 与参数开头 `{"` 的那一帧。Grok Build 随后以空工具名和缺开头的参数进行分发，报出 `Agent tried calling a tool that doesn't exist`，并把解析错误回喂给模型、消耗一轮重试（在经中继的 GLM 系渠道上反复出现）。
- **本次变化。** hellogrok 在共享的工具兼容层修复这类损坏，覆盖三种上游协议（`chat_completions`、`messages`、`responses`）及流式与非流式：
  - 参数缺开头时，补 `{"`（或 `{`）后若能解析为恰好一个完整 JSON 对象，则恢复前缀。
  - 空名称按该请求已声明工具的参数键集合推断；`command`+`description` 这一同时匹配 `run_terminal_command` 与 `monitor` 的共享形状确定性地解析为 `run_terminal_command`。仍无法判定时保持不修复，不做猜测。
  - 后续请求回放的已持久化历史（含 `/resume`）接受同样的修复，修复后的名称回填到对应 tool result 消息，损坏记录不再原样送达上游。

## 乱序与缺名工具块扣留到可解析为止

- **Chat Completions。** 工具帧改在流终止（`[DONE]`、流末或错误帧）发出，而非 `finish_reason` 时刻。Grok Build 的 chat 累加器读取整条流、不依赖 chunk 顺序，因此中继在 `finish_reason` 之后补发的参数片段会并入完整参数，而不是被静默丢弃留下尾部截断。持有中的行内推理仍在 `finish_reason` 时发出，思考与正文的可见延迟不变。
- **Messages。** `tool_use` 块在 `content_block_start` 与 `content_block_stop` 之间扣留；累积的 input JSON 先解析名称（并修复前缀），再以"开始帧 + 单条完整 `input_json_delta` + stop"重发。未闭合块在 `message_stop`/`error` 时冲刷。
- **Responses。** `function_call` 项在 `response.output_item.added` 与 `response.output_item.done` 之间扣留；done 帧的完整 item 同时作为名称与参数的第二来源，截断的流上参数让位于完整的终止 item。未闭合项在 `response.completed`/`incomplete`/`failed` 时冲刷。

## 诊断

- 提前 flush（错误终止）之后到达、因而被丢弃的工具调用增量，按调用索引记录为 `late-tool-deltas-discarded(index=N)`。中继在最后一个参数片段之前发出 `finish_reason` 的缺陷由此在代理日志中可见，而不是只表现为 Grok Build 的解析失败。

升级后请重启两个 hellogrok 可执行文件。
