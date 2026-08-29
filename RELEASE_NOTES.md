# Release Notes — v0.1.19

## Hosted-search billing totals no longer trigger false auto-compaction

Grok Build treats a terminal Responses `usage` block as the current live context. When a provider such as DeepSeek hosted Web Search reports cumulative billing tokens instead of the final prompt size, that number can jump above the model's `context_window` (for example `1.7M / 1.0M`) and Grok compact immediately, even though the real conversation still fits.

hellogrok now discards any prompt or output count larger than the known `context_window` and forwards `usage: null` instead. Grok keeps the previous baseline. In-window usage, including a provider `context_details` extension that itself fits the window, is unchanged. The check uses the configured model window, or a trustworthy upstream `X-Grok-Context-Window` header when the model has none.

Restart the proxy after upgrading. Keep an explicit `context_window` on custom models so this guard can run. Logs record discards as `usage discarded: live context exceeds window`.
