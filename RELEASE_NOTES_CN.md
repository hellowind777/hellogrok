# 发布说明 — v0.1.23

## Grok Build 本地能力面对齐

- 每个自定义渠道——Kimi、DeepSeek、GLM，以及被中转成 Responses、Messages 或 Chat Completions 的 grok-4.5/4.6——现在共用 Grok Build 的本地工具面：文件、终端、grep、子代理、客户端网页搜索、MCP（`search_tool`/`use_tool`）和 Skill。
- 编码会话还会声明 Claude/Codex 名称（`LS`、`Read`、`Bash`、`Task` 等）。调用会收回 `list_dir`、`read_file`、`run_terminal_command`、`spawn_subagent`。
- 直接 MCP 名（`server__tool`、`mcp__server__tool`）会包成 `use_tool`。Write 整文件写入、Glob 模式，以及缺失的必填 `description` / `subagent_type` 会补齐，让 Grok Build 能解析。
- 中转 grok-4.5/4.6 发出的 Grok Build 原名保持不变。历史里的 tool 结果消息会补上对应 `name`，避免思考模型在下一轮拒绝请求。

升级后请重启 hellogrok。未在当前请求中声明的工具不会被凭空创造；`x_search` 等 xAI 专属 hosted 工具仍排除。
