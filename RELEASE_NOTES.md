# Release Notes — v0.1.34

## Retryable Responses `response.failed` events are now absorbed instead of streamed

- **What you saw.** A relay under rate-limit or concurrency pressure returned a Responses SSE whose only terminal was a retryable `response.failed` (`rate_limit_exceeded`, `Concurrency limit exceeded`, overload). hellogrok streamed that failure to Grok Build immediately, so the turn failed even though a retry seconds later would have succeeded — and the retry then opened a second stream for the same request.
- **What changed.** While nothing has been written to the client, such a failed-only SSE is now withheld inside the absorb window (response headers and early frames stay buffered, up to 32 frames) and replayed with the same exponential backoff and `Retry-After` handling as HTTP soft failures. Grok Build keeps its full retry budget; only an exhausted window passes the failure through, still retryable. `absorb_retry_max_secs = 0` streams the failed event immediately. Deterministic `response.failed` events (authentication, invalid request/model) still stream through without retry.
- **Retry classification widened.** Concurrency-limit rejections (`concurrency`, `concurrency limit`, plus the corresponding Chinese phrases) now classify as transient, and error envelopes nested under `response.error` are recognized the same as top-level `error` objects.

## Client disconnects no longer become stream errors

- **What you saw.** Closing a Grok Build session mid-stream could leave a `proxy_stream_error` in the log or on a late reader, suggesting an upstream failure that never happened.
- **What changed.** Messages, Chat Completions, native, and Responses streams now distinguish a client abort (failed client write, canceled request context) from an upstream failure: aborts are logged as `aborted by client` and emit no stream error. Truly truncated upstream streams still emit `proxy_stream_error`.

## Strict errors for stream-shape mismatches

- An SSE response to a non-streaming request, or a streaming response to Grok Build's fixed non-streaming WebSearchClient request, now returns a non-retryable `502` naming the mismatch instead of forwarding an undecodable body.

## Quieter, safer diagnostics

- Upstream HTTP errors and Responses `response.failed`/`error` events are logged as structured summaries (`type`, `code`, `message`) with bearer tokens, key assignments, and `sk-` values redacted and long messages truncated.
- Benign upstream-model mismatches log once per channel/protocol/configured/upstream pair; conflicts and invalid declarations always log.
- When Grok Build sends an official catalog name such as `grok-4.6` on a non-xAI custom channel, the proxy logs a warning with body size, tool count, and session presence to aid `/resume` diagnosis. First-party `api.x.ai` routes and custom IDs such as `grok4.6-sevnx` are excluded.

Restart both hellogrok executables after upgrading.
