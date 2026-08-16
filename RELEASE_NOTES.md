# Release Notes — v0.1.11

## Tray exit always completes

Selecting **Exit** now attempts the full configuration restore and proxy shutdown, then always terminates hellogrok. A cleanup error can no longer leave the user trapped in the tray application. When the configuration cannot be accessed or a local route cannot be restored safely, the recovery transaction remains on disk so a later launch can retry before loading any routes.

Normal proxy-stop controls remain fail-closed when another provider manager owns Grok Build or an unowned local route would survive. This keeps routine stop behavior conservative without turning application exit into a permanent lock.

## Invalid TOML no longer blocks managed-field recovery

Users can edit `config.toml` while the proxy is active, including saving an incomplete value or text with a malformed byte-order mark. hellogrok now falls back to line-scoped recovery when the full document cannot be parsed: each managed assignment is compared independently, unchanged temporary values are restored, and user-owned bytes remain untouched.

The fallback still verifies that no `127.0.0.1:18787` channel route remains. If a renamed or structurally changed model retains such a route and ownership cannot be proven, hellogrok preserves both the configuration and its recovery transaction instead of applying an unsafe rewrite.
