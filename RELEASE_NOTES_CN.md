# 发布说明 — v0.1.33

## 已完成的中转流不再因省略协议尾帧而被判失败

- **不绑某个渠道或模型。** 按协议处理，不看供应商名字。所有自定义渠道都适用：Chat Completions、Messages、Responses，中转侧用什么模型 ID 都一样。
- **你看到的现象。** Grok Build 显示 `Server error | Retrying (attempt N)`，随后变成 `Server error: Something went wrong on our side. Wait a minute and send again.`，而中转后台一直是 HTTP 200。黄字横幅把真实原因藏掉了：流「没有终态事件」的 `proxy_stream_error`。
- **实际发生了什么。** 很多中转会返回完整 SSE 流（Chat 的 `finish_reason`、Messages 的 `stop_reason`、Responses 的 output item 或已 completed 的 `status`），然后不发 Grok Build 解码器要的尾帧（`[DONE]`、`message_stop`、`response.completed`）就关连接。Chat/Messages 在干净关闭时 Grok 自己能收尾；Responses 不能。此前 hellogrok 一律注入 `proxy_stream_error`，Grok Build 最多重试 15 次——每次都在中转侧再记一笔 200——预算耗尽后打出通用 Server error。重试倒计时是退避，不是首字超时。
- **修复。** 干净关闭时按协议补发缺失尾帧，而不是把流改写成错误。真正的截断（没有任何停止信号也没有任何输出）、空闲超时和读失败仍发 `proxy_stream_error`。

升级后请重启两个 hellogrok 可执行文件。
