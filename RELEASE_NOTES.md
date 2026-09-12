# Release Notes — v0.1.25

## Tolerate upstream ID reuse in Responses and Chat Completions streams

- sub2api-style gateways and some relays reuse one item ID across Responses output slots (for example a single reasoning ID per response) or one call ID across parallel Chat Completions `tool_call` items. Previously hellogrok's identity validation rejected the stream, Grok Build retried until its budget was exhausted, and the turn ended as `Server error (500)`.
- Responses streams now remap the colliding later slot to a fresh unique ID that keeps the provider's type prefix (`rs_`, `ws_`, `msg_`, …). Every later `item_id` event for that slot — reasoning summary deltas, function-call argument deltas, and the final `response.completed` snapshot — is rewritten consistently, so Grok Build receives a self-consistent stream and the conversation survives.
- Chat Completions streams now assign a unique `call_` ID to any `tool_call` whose upstream ID was already used by another call in the same response, keeping the next round's tool history valid.
- The remap is counted and logged per stream (`remapped N duplicate upstream item id(s)`) so a buggy upstream remains visible in diagnostics instead of failing silently.
- Genuine same-slot identity conflicts — a slot that switches to a brand-new unrelated ID — are still rejected. The tolerance covers only the confirmed ID-reuse pattern; request-side tool-history validation is unchanged and continues to protect the next turn from corrupted IDs.

Restart hellogrok after upgrading and start a new session. Stored conversation bubbles are not rewritten.
