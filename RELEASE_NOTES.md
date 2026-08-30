# Release Notes — v0.1.20

## Transient upstream "busy" failures no longer kill the turn

Third-party channels often answer overload with `Service unavailable (503): The service is busy. Wait a minute and send again.` The previous circuit breaker treated a short streak of those as a dead channel and force-failed the turn within seconds, wasting Grok Build's native retry budget (15 attempts, about 5.5 minutes). Long agent turns died as soon as the provider hit a busy window.

hellogrok now keeps the client's full retry budget intact and adds an absorb layer in front of it:

- Retryable soft failures before response headers (busy/overloaded `503`, `429`, retryable `5xx`, response-header timeouts) are retried inside the proxy with exponential backoff (2s–30s, honoring upstream `Retry-After` up to 60s) for up to 90 seconds by default. Grok Build never sees the failure while the window lasts. `absorb_retry_max_secs = 0` disables it per channel; `absorb_retry_backoff_cap_secs` tunes the backoff.
- When the window is exhausted, the original failure passes through as retryable, so the client still has all 15 retries. Retryable busy `5xx` responses without an upstream `Retry-After` get a synthesized 30-second one to pace the client.
- Transport errors are deliberately not absorbed: Grok Build's first-retry HTTP/1.1 client rebuild handles them better than a proxy-internal replay could.
- `X-Should-Retry: false` is now reserved for deterministic failures whose retries cannot succeed (authentication, permission, billing, quota, invalid request/model). Nothing else ever shortens the client's retry budget.
- The circuit breaker becomes opt-in (`dead_channel_fail_fast = true`, default off) and counts only dial-level failures — connection refused, DNS, TLS handshake — which never recover on retry. After 6 consecutive failures (configurable via `dead_channel_fail_threshold`) the proxy answers a non-retryable `503 proxy_circuit_open`, with one probe after a 5-minute cooldown and any upstream response closing the breaker. Busy `503`, `429`, and timeouts never count.

Restart the proxy after upgrading. Each absorb wait is logged as `UP absorb`; dead-channel fast-fails appear as `UP breaker` lines.
