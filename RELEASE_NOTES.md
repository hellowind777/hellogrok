# Release Notes — v0.1.16

## Reasoning configuration remains user-owned

hellogrok no longer creates, migrates, reorders, or replaces `reasoning_effort`, `reasoning_efforts`, or `supports_reasoning_effort` for any channel. Users may configure a selected level, a reasoning menu, both, or neither; missing fields continue to use Grok Build's model catalog and provider defaults.

Recovery remains compatible with temporary reasoning projections recorded by earlier releases, so stopping or recovering the proxy can still remove those obsolete managed values without changing current user-owned settings.

## Provider reasoning levels pass through unchanged

Responses, Chat Completions, and Messages now preserve provider-owned reasoning levels instead of normalizing them through a DeepSeek-specific mapping table. Unknown or future non-empty levels reach the provider unchanged and remain subject to that provider's validation.

Responses-to-Messages conversion follows Grok Build's native serializer: `none` and `minimal` do not produce Messages reasoning fields, while every other non-empty value is retained.

## DeepSeek off selections keep their meaning

On the first-party DeepSeek endpoint, an explicit `none` selection is converted to the native thinking-off switch for Chat Completions and Messages. This also works when `reasoning_effort = "none"` is the only reasoning field in the model configuration; a reasoning menu is not required.

When no reasoning fields are configured, hellogrok does not add a thinking switch and leaves Grok Build and DeepSeek defaults unchanged.
