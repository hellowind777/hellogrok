# Release Notes — v0.1.23

## Grok Build local capability alignment

- Every custom channel — Kimi, DeepSeek, GLM, and relayed grok-4.5/4.6 in Responses, Messages, or Chat Completions — now shares Grok Build's local tool surface: files, shell, grep, subagents, client web search, MCP via `search_tool`/`use_tool`, and skills.
- Coding sessions also advertise Claude/Codex names (`LS`, `Read`, `Bash`, `Task`, …). Calls come back as `list_dir`, `read_file`, `run_terminal_command`, `spawn_subagent`.
- Direct MCP names (`server__tool`, `mcp__server__tool`) become `use_tool`. Write-style full-file payloads, Glob patterns, and missing required `description` / `subagent_type` fields are filled so Grok Build can parse them.
- Exact Grok Build names from relayed grok-4.5/4.6 are left unchanged. History tool-result messages receive the matching `name` so thinking models do not reject the next turn.

Restart hellogrok after upgrading. This does not invent tools that were not declared, and xAI-only hosted tools such as `x_search` stay excluded.
