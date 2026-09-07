# Release Notes — v0.1.21

## Preserve Grok Build session identity for OpenCode Go

OpenCode Go now requires `x-opencode-session` for request routing. Grok Build sends stable conversation identity under its own headers, so a custom channel could continue working in a direct client while failing when the same channel was proxied by hellogrok.

hellogrok now normalizes the incoming conversation identity internally and projects it at the upstream boundary for OpenCode Go channels:

- Uses an existing non-empty `x-opencode-session` first.
- Falls back to `x-grok-conv-id`, `x-grok-session-id`, `x-session-id`, `session_id`, or `metadata.session_id`.
- Does not add configuration fields or generate a new per-request UUID.
- Limits the projection to `opencode.ai/zen/go` routes; other providers, including official xAI and DeepSeek routes, are unchanged.

Restart hellogrok after upgrading, then reselect the affected custom model in Grok Build if its active model entry was already loaded before the upgrade.
