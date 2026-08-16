# Release Notes — v0.1.15

## Existing reasoning and search fields keep their positions

Enabling the proxy no longer changes the relative placement of existing `reasoning_effort`, `reasoning_efforts`, or `supports_backend_search` assignments. Single-line and multiline reasoning menus are updated in place, while comments, unrelated model settings, and user-defined field order remain intact.

This behavior is shared by every proxied channel. Reapplying the active projection produces identical configuration bytes, and a normal stop restores the original assignments and formatting exactly.

## Missing capability fields use a stable order

When one or more of these fields must be projected but are absent, hellogrok now uses existing user assignments as anchors and inserts only the missing values in this order: `reasoning_effort`, `reasoning_efforts`, `supports_backend_search`.

The same ordering applies when all three fields are absent. One-time migration of an exact legacy hellogrok reasoning menu still compacts that menu, but it preserves the relative placement of existing search and reasoning settings.
