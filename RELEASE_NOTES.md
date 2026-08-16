# Release Notes — v0.1.14

## Managed configuration writes now use UTF-8 without BOM

hellogrok continues to read UTF-8 `config.toml` files with or without a byte-order mark, and read-only inspection never rewrites the file. Whenever the proxy successfully applies, restores, or rolls back managed settings, the atomic write now saves the configuration as UTF-8 without BOM.

If a user adds a BOM while the proxy is active, a later successful restore removes it while preserving concurrent field edits. Valid UTF-8 text remains eligible for line-scoped recovery when unrelated edits temporarily make the TOML document invalid; a successful recovery also writes the result without BOM.

## Invalid encodings fail closed during recovery

Line-scoped recovery now runs only after the configuration has passed UTF-8 validation. Invalid UTF-8 reports the configuration path and first invalid line, column, and byte offset without exposing configuration values. The configuration bytes and recovery state remain unchanged so restoration can be retried after the encoding is corrected. hellogrok does not guess or transcode GBK, UTF-16, or another encoding.
