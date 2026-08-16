# Release Notes — v0.1.12

## Safe auto-compaction for every custom model

hellogrok now calculates an independent auto-compaction budget for each custom model. It reserves the model's maximum output plus a 5% context margin with an 8K minimum, then temporarily lowers only thresholds that would compact too late. A lower user threshold is never raised, and switching channels reloads the selected model's own budget instead of sharing a global minimum.

This corrects the failure mode where Grok Build applies `auto_compact_threshold_percent` to the full `context_window` without first subtracting `max_completion_tokens`. For a 1,048,576-token window and a 384,000-token maximum output, the default 85% preference is safely limited to 58%.

## Capacity learning without model-name rules

Explicit model or provider limits remain authoritative. When a capacity field is missing, hellogrok can learn it from the actual outgoing output allowance, valid upstream capacity headers, or one unambiguous structured context-limit error. A trusted learned context window is temporarily projected at model level so Grok Build uses the same denominator; a request-derived output value participates in budgeting but never becomes a model output cap.

Learned records use hashed route identities, contain no URL, model name, or credential, and expire after 30 days. If one or both capacity values remain unknown, hellogrok reports that the model is still learning and leaves its threshold unchanged instead of guessing from its name.

## Transactional updates remain removable at any time

Runtime capacity changes use the existing recoverable configuration transaction and refresh idle Grok sessions. Active-session retries are bounded, coalesced, and cancellable; proxy shutdown cancels and joins both background workers before restoring configuration, so the new automation cannot prevent tray Exit from completing.

Windows sharing conflicts during an atomic update are retried briefly. If an update still cannot be committed, hellogrok keeps the previous active proxy configuration and its matching recovery state rather than rolling the whole proxy back underneath the running process. Normal shutdown continues to restore original bytes and preserve concurrent user edits, including edits made while the TOML document is temporarily invalid.
