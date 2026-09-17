# Release Notes — v0.1.39

## Long relay stalls no longer kill the whole turn

- **What you saw.** Some relay channels (GLM and others) stream heartbeat bytes for many minutes without producing any content while queued or thinking — stalls of 3+ minutes with heartbeat-only frames have been observed. Grok Build's content-progress timer only counts real content (text, reasoning, tool-call deltas, terminal signals); heartbeats and empty deltas do not reset it. After 600 seconds without content it ends the turn with `IdleTimeout`, which is classified **non-retryable** — the 15-attempt budget never engages and the task dies.
- **What changed.** Two layers of protection:
  1. **Managed idle deadline.** Proxied channels without an `inference_idle_timeout_secs` value now get 1800 seconds materialized temporarily (restored byte-for-byte on stop; first-party `api.deepseek.com` routes are excluded so their remote metadata stays authoritative), widening the tolerated single stall from 10 to 30 minutes. Explicit per-model or global `[models]` values remain user-owned and are never rewritten.
  2. **Downstream content watchdog.** Every streaming path (native Chat/Messages/Responses passthrough and the protocol-translation projections) mirrors Grok Build's per-protocol content classification and fires one 30-second margin ahead of the client's own content timer. On fire the proxy closes the upstream body and emits a **retryable** `proxy_stream_error`, so a genuinely stalled stream re-enters Grok Build's native 15-attempt retry budget instead of hitting the non-retryable client classification. Watchdog activity is logged as `SSE stalled: no content reached Grok Build for …`. Explicit timeouts below 60 seconds disable the watchdog to respect fast-fail choices.

## Net effect

Together with the existing soft-failure absorb window, a single request now survives: transient pre-header failures (retried inside the proxy), long heartbeat-only stalls (waited out up to ~30 minutes), and a truly stuck stream (converted to a retryable error with up to 15 client retries). Unattended long-running tasks no longer break on relay jitter.

Restart both hellogrok executables after upgrading. If you previously set `inference_idle_timeout_secs` for a channel, your value is kept as-is; channels without one log `inference idle timeout model=… managed=1800s` at startup.
