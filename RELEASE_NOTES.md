# Release Notes — v0.1.29

## Upstream 408s and short relay outages no longer kill unattended turns

- **Upstream `408` is treated as what it really is.** Between two servers there is no "client was too slow" — a relay or Cloudflare edge emits 408 for an edge-side timeout while the origin is queued or restarting, semantically a 504. Grok Build classifies a literal 408 as terminal and fails the turn on first sight. hellogrok now retries a 408 inside the absorb window and remaps an exhausted one to a retryable `504`, so the client keeps its native retry budget instead of dying. This was the exact failure that interrupted long-running agent turns on busy relays.

- **Soft-failure absorption is always on, deterministic 4xx is configurable.** Retryable `5xx`, `429`, 408 edge-timeout pages, Cloudflare origin-TLS `525`/`526`, and response-header timeouts are absorbed for every channel, because those clear on their own and the client's retry of them only burns time. Deterministic rejections (authentication, permission, invalid request/model) pass through immediately with the provider's explanation by default. A new global `[models]` `error_resilience = "balanced"` setting additionally absorbs those 4xx failures with a fixed 30s-then-60s wait sequence, so an unattended turn survives relay-side token rotation, permission fixes, config reloads, and brief deploys. A reasoning-history rejection is never absorbed at any setting: it reports a foreign conversation state the user must see.

- **A long-hidden failure says how stale it is.** An error that outlasted a long absorb window is the last attempt's snapshot, not a live one. hellogrok now stamps `X-Hellogrok-Absorb-Delay` on the passthrough response and prefixes structured error messages with a one-line `[hellogrok: upstream stayed failing for ...]` note, so the delay is visible instead of masquerading as a fresh failure. Once the window is exhausted, Grok Build's own retry indicator shows the error reason (`<headline> | Retrying (N/M)`) while its native budget continues — the failure never hides behind a bare retrying state.

## Relay TLS failures now reach the dead-channel breaker

The opt-in breaker (`dead_channel_fail_fast = true`) only counts dial-level failures, but its TLS classification covered only certificate-verification errors. It now counts every handshake-failure form Go actually produces — remote/local alert errors, `tls.RecordHeaderError`, and the HTTPS-to-plain-HTTP scheme mismatch — so a channel with broken TLS termination fast-fails instead of burning the full client retry budget.

## Responses thought-gate hardening

- **Post-answer reasoning can no longer leak through the `output_item.done` frame.** The event now honors the same drop/strip rules as its `added` counterpart; `reasoning_summary_part.added/done` text goes through the protocol self-talk filter; a post-answer `<think>`-only content part is dropped.
- **Relay thinking variants are gated, not leaked.** Items and content parts typed `thinking`, `reasoning_summary`, or `redacted_thinking` — what relays that bridge Anthropic-style blocks through protocol conversion emit — pass through the same sanitize/drop rules as official `reasoning`, in both streamed frames and the terminal `response.completed` frame.
- **Fail-loud on the unknown.** The gate is now table-driven with one registered rule per event type; an unregistered event that visibly carries reasoning is intercepted and logged instead of leaking a second Thought under the reply. Unrelated unknown events keep passing untouched.

## Configuration-recovery fixes

- Invalid-TOML recovery no longer deletes a user-edited root `subagents.enabled` dotted line, and drops management of feature-flag lines whose entire `[features]` section the user deleted — both now match the parse path. A new invariant test pins the two restore paths to the same decision for the same input.
- Requests caught mid-flight by a proxy stop receive the structured `503 proxy_stopped` diagnostic instead of a one-off retryable `502`, so stale sessions know to reselect a model.

Every change stays inside the existing safety contract: repairs happen before Grok Build's strict validators see the wire, deterministic refusals remain non-retryable so a rejected request never re-enters a retry loop, and `error_resilience` is the only new configuration — optional, global, off by default.

Restart both hellogrok executables after upgrading.
