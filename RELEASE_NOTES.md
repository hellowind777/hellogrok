# Release Notes — v0.1.7

## Active configuration edits no longer block safe shutdown

Stopping the proxy or exiting the tray now performs a field-level three-way merge of proxy-managed configuration. Fields that still contain hellogrok's temporary values are restored to their startup state, while values changed during the active session are preserved.

This fixes the case where changing `supports_backend_search`, a channel URL, a feature flag, or the subagent setting while the proxy was running could leave the tray unable to stop or exit. Deleting an entire model channel remains an explicit user change and is also preserved.

## Temporary route protection remains enforced

Before stopping the server, hellogrok validates the merged configuration for temporary routes introduced by the current takeover. A renamed or moved model that still points to `127.0.0.1:18787` keeps the proxy active instead of leaving Grok Build connected to a stopped endpoint. Invalid TOML and active external configuration ownership continue to defer shutdown.
