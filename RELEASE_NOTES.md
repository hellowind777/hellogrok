# Release Notes — v0.1.28

## Relay wire quirks no longer kill turns that a retry would fix

- **Failures wrapped in HTTP 200.** Some newapi/sub2api relays answer a failure with a success status plus an error envelope. Previously that body fell into envelope validation and died as an opaque, non-retryable rejection. hellogrok now detects the wrapped envelope: transient semantics (rate limit, overloaded, timeout) enter the absorb window and are retried inside the proxy; deterministic semantics (authentication, billing, invalid request) pass through with the provider's own explanation. A Responses terminal body that carries its own error member keeps its native failed-response path, so genuine `response.failed` bodies are untouched.
- **Cloudflare shield challenges.** A challenge page in front of a relay (`cf-mitigated: challenge`, or a Cloudflare-served "Just a moment..." page) is transient by nature. It is now absorbed inside the retry window; a challenge that outlasts the window passes through as a retryable `503`, because a bare `403` classifies terminal in Grok Build and would kill the turn on a self-clearing shield. A genuine origin `403` without Cloudflare markers is untouched.
- **Duplicate Chat tool-call IDs in non-streaming responses.** The streaming rectifier already remapped a relay-reused `tool_call` ID; the non-streaming path rejected the same repairable defect. Both paths now remap to a fresh unique ID, keeping the next round's tool history valid instead of failing the turn.
- **Missing Responses envelope bookkeeping.** Relays that omit `id`, `object`, or `status` on an otherwise complete terminal body now get the missing markers synthesized instead of a 502 rejection. Present-but-wrong values and a missing or malformed `output` remain rejections.

Every new tolerance stays inside the existing safety contract: repairs happen before Grok Build's strict validators see the wire, refusals remain non-retryable so a rejected request never re-enters a retry loop, and no new configuration is required. Regression tests cover absorb-then-recover and passthrough for each new path.

Restart both hellogrok executables after upgrading.
