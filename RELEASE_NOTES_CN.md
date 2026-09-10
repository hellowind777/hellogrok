# 发布说明 — v0.1.22

## 工具调用兼容

- 在 Chat 和 Messages 响应交给 Grok Build 前补齐缺失的本地工具调用 ID。
- 保持 Responses 流中输出项和函数调用的跨事件身份一致，拒绝冲突身份。
- 上游返回 JSON、客户端需要 Chat SSE 时，为并行工具调用分配独立索引。
- 已有 Chat 历史仅在缺失调用 ID 可与唯一结果关联时修复；有歧义的历史仍需新建会话。

## OpenCode Go 与 Zen

- 从原始请求保留身份，覆盖 Responses、Messages、Chat、搜索转换及内部重试。
- 将 `x-opencode-session` 适配扩展到官方 Go 和 Zen 路由。客户端未提供身份时，使用独立操作 ID 继续转发，不再因缺少身份被本地拒绝；内部重试复用同一 ID。这不会恢复不同请求间的对话路由或缓存连续性。
- User-Agent 优先保留显式配置，其次使用入站客户端标识；两者均缺失时，从本机 `grok --version` 生成 Grok Build 标识。

## Windows

状态与日志窗口标题现在包含正在运行的 hellogrok 版本号。

升级后请重启 hellogrok。供应商侧搜索及其他能力仍需上游真实支持；这些修复不代表所有第三方模型都支持 Grok Build 的每一种工具。
