# Release Notes — v0.1.33

## Completed relay streams are no longer failed for omitting a wire trailer

- **Not tied to one channel or model.** The repair is per protocol, not per provider name. It applies to every custom channel: Chat Completions, Messages, and Responses, whatever model ID the relay uses.
- **What you saw.** Grok Build showed `Server error | Retrying (attempt N)` and then `Server error: Something went wrong on our side. Wait a minute and send again.`, while the relay dashboard stayed HTTP 200. The yellow banner hid the real cause: a `proxy_stream_error` that the stream had ended without a terminal event.
- **What was actually happening.** Many relays return a full SSE stream (Chat `finish_reason`, Messages `stop_reason`, Responses output items or a completed `status`) then close without the trailer Grok Build's decoder wants (`[DONE]`, `message_stop`, `response.completed`). Grok Build can complete Chat/Messages on a clean close; Responses cannot. hellogrok previously injected `proxy_stream_error` in every case, which Grok Build retries up to 15 times — each retry a new billed 200 — then surfaces the generic server-error copy. The retry countdown is backoff, not a first-token timeout.
- **The fix.** On a clean close, hellogrok synthesizes the missing trailer for that protocol instead of rewriting the stream as an error. Truly truncated streams (no stop signal and no output), idle timeouts, and read failures still emit `proxy_stream_error`.

Restart both hellogrok executables after upgrading.
