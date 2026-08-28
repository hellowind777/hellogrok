# Release Notes — v0.1.18

## Channels with a dead relay fail fast instead of stalling

Grok Build retries retryable statuses (429, 5xx) up to 15 times, roughly 5.5 minutes per turn. When a relay's origin stayed down, every turn burned that entire retry budget and the UI stayed in the "retrying" phase, making the channel look permanently unusable.

hellogrok now keeps an independent circuit breaker per channel. After 4 consecutive retryable upstream failures (5xx, transport errors, or error-body read failures), the proxy stops forwarding and immediately answers a non-retryable `503 proxy_circuit_open` with `X-Should-Retry: false`, so Grok Build fails the turn right away. After a 90-second cooldown one probe request is allowed through: success closes the breaker automatically and the channel keeps working, failure re-arms the cooldown. Any non-5xx upstream response, including 429, resets the failure streak. Streaming failures that occur after response headers are sent are unaffected.

When `proxy_circuit_open` appears, wait about 90 seconds and retry for a temporary outage; if the error repeats, the relay's origin is down for the long term and the channel should be switched with `/model`. The proxy log records breaker transitions as `UP breaker` lines.
