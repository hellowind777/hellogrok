# Release Notes — v0.1.38

## Provider usage reports now survive transport format quirks

- **What you saw.** Some relay channels (GLM, and potentially others served by one-api/new-api-style gateways) report token usage with non-standard JSON types — counts as strings (`"prompt_tokens": "12345"`), whole floats, or invalid optional fields (null detail values, fractional costs). hellogrok previously dropped the entire usage measurement on any format defect, forcing Grok Build onto its byte-based estimate. For Chinese and JSON-heavy conversations that estimate overshoots real token counts by 25% or more, so auto-compaction fired far too early — at roughly 50% real usage instead of the configured threshold. The premature summary dropped all tool results, the model re-read files it had already processed, context ballooned again, and a second compaction stalled the session entirely.
- **What changed.** A usage rectifier now repairs provider measurements before they reach Grok Build's token ledger and auto-compaction meter. Stringly-typed counts, whole floats, and `json.Number` values are normalized to integers; invalid optional decorations (null detail values, non-map detail containers, fractional `cost_in_usd_ticks`) are removed without poisoning the core measurement; unrecoverable core counts (negative, fractional, overflowing, non-numeric) are deleted so the downstream validator drops the measurement instead of trusting corrupt data. Repairs apply across Chat Completions, Responses, and Messages, in streaming and non-streaming responses, and in protocol-translation paths, and are logged as `usage rectified` proxy notes so you can see exactly what was fixed.

## Stream rectifier architecture unified

- The three frame-rewriting stream rectifiers (Chat tool calls, Messages tool blocks, Responses function calls) now share a common `streamFrameRectifier` contract, making it straightforward to add rectification for future protocols without touching the dispatch pipeline.

Restart both hellogrok executables after upgrading. If you use a GLM channel, watch the proxy log for `usage rectified` notes — they confirm the channel's usage reports were previously being dropped and are now reaching Grok Build correctly.
