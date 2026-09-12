# 发布说明 — v0.1.25

## 容忍上游在 Responses 与 Chat Completions 流中复用 ID

- sub2api 类中转网关和部分渠道会把同一个 item ID 复用到 Responses 的不同输出槽（例如整条响应共用一个 reasoning ID），或把同一个 call ID 复用到 Chat Completions 的并行 `tool_call` 上。此前 hellogrok 的身份校验会直接拒绝这条流，Grok Build 重试到预算耗尽后，整轮以 `Server error (500)` 失败。
- Responses 流现在为后到的冲突槽位改写一个保留上游类型前缀（`rs_`、`ws_`、`msg_`……）的新唯一 ID。该槽位后续所有携带 `item_id` 的事件——reasoning 摘要增量、函数调用参数增量、以及最终的 `response.completed` 快照——都会被一致改写，Grok Build 收到的是一条自洽的流，会话不再中断。
- Chat Completions 流现在为同一条响应中与先前调用撞 ID 的 `tool_call` 分配唯一的 `call_` ID，保证下一轮的工具历史仍然有效。
- 每条流的改写次数都会计数并写入日志（`remapped N duplicate upstream item id(s)`），让有缺陷的上游在诊断中仍然可见，而不是静默失败。
- 同一槽位内的真实身份冲突——某槽位突然换成一个全新无关的 ID——仍然会被拒绝。容忍只针对已确认的上游 ID 复用模式；请求侧的工具历史校验保持不变，继续保护后续轮次不被脏 ID 污染。

升级后请重启 hellogrok 并新开一轮会话。已存盘的对话气泡不会改写。
