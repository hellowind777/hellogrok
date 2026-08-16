# Release Notes — v0.1.13

## UTF-8 BOM no longer blocks startup

hellogrok now accepts `config.toml` as UTF-8 with or without a byte-order mark. A standard BOM no longer fails the configuration-ownership check before the proxy starts.

Normal proxy transactions preserve the file's current BOM choice. If the user adds or removes the BOM while the proxy is active, shutdown keeps that edit while restoring only hellogrok-managed settings. Runtime state, preference, and capacity-cache files owned by hellogrok remain UTF-8 without BOM.

## Actionable configuration errors

TOML syntax errors now identify the configuration file and include the parser's line and column. Invalid UTF-8 reports the first invalid location without printing configuration values. hellogrok does not guess or silently transcode GBK, UTF-16, or another encoding.

## Explicit encoding normalization

The new `hellogrok normalize-config` command validates the complete TOML document and atomically removes a valid UTF-8 BOM. It refuses to run against invalid content, an active hellogrok proxy, or an active Grok Build takeover by another provider manager. Ordinary startup never changes the file's encoding silently.
