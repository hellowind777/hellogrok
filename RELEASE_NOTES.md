# Release Notes — v0.1.40

## Shorter managed stall window, uniform across channels

- **Managed `inference_idle_timeout_secs` lowered from 1800 to 900 seconds.** The 30-minute ceiling traded too much failure-detection latency for stall tolerance. Fifteen minutes still covers twice DeepSeek's documented ten-minute queue and multiples of the observed three-minute relay stalls, and the downstream watchdog (deadline minus 30 seconds) already converts a true stall into a retryable `proxy_stream_error`, so the extra wait mostly delayed discovery.
- **The managed timeout now applies to every proxied channel, including first-party `api.deepseek.com` routes.** It is a resilience projection, not channel metadata, so it is no longer special-cased. Explicit per-model or global `[models]` values remain user-owned and win on every channel alike. The proxy's DeepSeek-specific 660-second upstream byte window is unaffected.

Restart both hellogrok executables after upgrading; the rewritten channel values take effect at the next proxy start. Channels you configured explicitly keep their value unchanged.
