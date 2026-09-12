# Release Notes — v0.1.27

## Relay origin-TLS failures (525/526) no longer kill a turn on first sight

- `Server error (525): Something went wrong on our side.` is Grok Build's generic copy for any 5xx: the status came from the relay's Cloudflare edge, which failed the TLS handshake with the relay's own origin (525) or rejected its origin certificate (526). The request never reached the relay's application, and Grok Build classifies both statuses as terminal, so the turn died on the first occurrence even when the relay recovered seconds later.
- hellogrok now retries 525/526 inside its absorb window (default 90s, per-channel `absorb_retry_max_secs`). Replaying is side-effect free because a request that failed the edge-origin TLS handshake never reached the origin application. A relay origin restart or certificate rotation that clears inside the window is now invisible; when the window is exhausted the failure passes through with the terminal disposition Grok Build expects.
- The passthrough retry disposition now mirrors Grok Build's edge-client policy: `429` and every `5xx` except the origin-TLS codes are retryable. Previously hellogrok stamped `X-Should-Retry: false` on transient Cloudflare edge pages (`520`–`524`, `529`, `530`), and that header vetoes Grok Build's own retry of a failure that clears on its own — turning a self-healing edge blip into an instant turn failure.
- Regression tests cover absorb-then-recover on 525, terminal passthrough after the window, and the retryable disposition of edge pages.

Restart both hellogrok executables after upgrading. A channel that reports 525 repeatedly has a misconfigured origin certificate or TLS termination that only the relay operator can fix.
