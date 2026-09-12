# Release Notes — v0.1.26

## Response-header timeouts now follow the designed retry path

- A response-header timeout used to cancel the context shared by the whole retry loop. The absorb layer's wait then returned immediately and the handler finished without writing anything, so the client received an empty `200` instead of the designed retryable `504`, and absorb retries after a header timeout were a dead path.
- The timeout now cancels only the failing attempt's derived context. A header timeout produces the retryable `504`, the absorb layer actually retries within its configured budget, and a successful absorb retry surfaces to the client as a normal response. A healthy long-running stream is no longer cut short by attempt-level bookkeeping.
- Regression tests cover both paths: the retryable `504` write and a successful absorb retry after a header timeout.

## Dead-channel breaker probes can no longer latch open forever

- A probe that ended without reporting an outcome — the caller disconnected mid-probe, or the probe's response headers timed out — never reached the success/failure recording, so the breaker stayed in probing state and the channel kept fast-failing permanently even after connectivity returned.
- Each probe now carries a 2-minute lease. When the lease expires without a reported outcome, the probe is presumed lost and the next request releases a fresh one. Probes that do report an outcome close or re-arm the breaker exactly as before.

## Concurrent `config.toml` edits are preserved during apply and restore

- Apply and restore re-read `config.toml` immediately before the atomic rename. If another program changed the file after hellogrok read it for the current operation, the write aborts before touching the file with a retryable error, the external edit is preserved, and the rewrite state stays on disk so the operation can simply be retried.
- Atomic config writes now also flush the renamed file's directory entry, so a power loss cannot lose an already committed rename (best-effort on filesystems without directory sync).
- Recovery removes a hellogrok-created `subagents.enabled` dotted key only from the file's root table. The user's own `subagents.enabled` inside any other table — for example `[model.one]` — is no longer mistaken for a temporary projection and deleted.

## Recovery and derived state no longer deadlock on corrupt or missing files

- When `config.toml` no longer exists, there is nothing left to restore: `hellogrok restore` now drops the obsolete recovery record instead of failing every later start and restore on a record that can never succeed. An unsupported rewrite-state format error now names the state file to delete.
- A corrupt `capacity_cache.json` or preferences file no longer blocks startup or every later read/write. The unparseable file is preserved next to the original with a `.bad` suffix, and the application continues with an empty capacity cache or default preferences; both files are purely derived state and rebuild themselves from live traffic and the next write.

## Windows console and log-window robustness

- Console allocation failures other than "a console already exists" now surface as errors, and a failure to bind `CONOUT$` / `CONIN$` returns an error instead of silently leaving stdio unbound.
- Repeated **Status and logs** clicks while the window is still being created no longer start a second creation attempt; if creation failed or timed out, a later click can retry. The creation-timeout error now points to the actual log-file path.

Restart both hellogrok executables after upgrading, then start a new session. Stored conversation bubbles are not rewritten.
