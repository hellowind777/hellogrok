# Release Notes — v0.1.24

## Native Chat alignment with Grok Build

- Ordinary Chat and Messages channels stay on Grok Build's first-party mapper. Responses projection is used only for hosted search and WebSearchClient.
- Native Chat SSE reassembles tool-call deltas before last-write-wins: empty later `"name"` fields no longer erase a known tool, and incomplete argument JSON is not rewritten per frame.
- Empty or vendor `finish_reason` values are dropped or mapped onto `stop` / `length` / `tool_calls` / `content_filter` / `function_call`.
- Thought is a prefix sibling only. Post-answer CoT, `<think>` in content, and protocol self-talk (`reply only:` / `任务已全部完成`) are stripped. Reasoning deltas keep BPE leading spaces.
- Chat history keeps intra-turn tool-loop `reasoning_content`, strips cross-turn plaintext CoT except DeepSeek/MiMo, and never injects thinking placeholders. Encrypted or signed blobs pass through.
- Upstream tool lists keep Grok Build `client_name` values. Claude/Codex aliases are rewritten on inbound calls only.
- Every upstream request is presented as `grok-shell`. Streaming Chat keeps `include_usage`; GLM Chat with tools sets `tool_stream=true` when omitted.

Restart hellogrok after upgrading and start a new session. Stored bubbles are not rewritten. This does not invent tools that were not declared.
