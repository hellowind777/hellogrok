# Release Notes — v0.1.8

## Grok Build auto-compaction now receives trustworthy context usage

hellogrok no longer turns missing or incomplete provider usage into a valid-looking zero measurement. Responses, Anthropic Messages, and Chat Completions now preserve complete input, output, total, cache, and reasoning token data; unusable measurements become `usage: null`, allowing Grok Build to retain its existing token baseline instead of resetting auto-compaction progress on every turn.

Explicit model or provider `context_window` values remain authoritative and cannot be replaced by conflicting upstream metadata. When neither is configured, provider or Grok Build model metadata remains in control. `max_completion_tokens` continues to limit output only and is not used as the auto-compaction denominator.

## DeepSeek compatibility follows the first-party endpoint and wire protocol

Routes on the exact `api.deepseek.com` host now receive first-party Responses, Chat Completions, and Anthropic Messages handling without a model-ID allowlist. Rolling aliases and future model IDs therefore inherit the same authentication, endpoint, hosted-search, reasoning, usage, queue keep-alive, and streaming behavior while model capacities remain configuration- or metadata-driven.

The current DeepSeek effort mapping is available in every supported format, including an explicit non-thinking mode. Tool-call reasoning history, structured output, function tools, native hosted Web Search, explicitly supplied custom `apply_patch` requests, terminal failure events, and the canonical Anthropic endpoint are normalized according to the target protocol. Unsupported fields such as Responses `include` are not invented.

## Search and Grok Build tools remain available across provider formats

Every provider format can carry Grok Build's local function tools and their parallel call/result history. A channel with `supports_backend_search = true` receives its native hosted-search request; selecting a custom default search model temporarily enables the same route even when the channel setting is false. An upstream that rejects or silently ignores hosted search now returns an explicit failure instead of being reported as a successful `web_search`.

Native and bridged requests preserve deterministic conversation, tool, reasoning, and cache prefixes. Cache token details returned by the provider remain visible, while hellogrok does not claim a cache hit when the upstream does not report one.

## Long-running provider requests have bounded idle behavior

Response-header and response-body idle protection now covers streaming and non-streaming success and error bodies. Ordinary routes stay just behind Grok Build's configured inference idle timeout, while first-party DeepSeek routes allow the documented queue window and renew the deadline on queue newlines or SSE keep-alives. A silent upstream can no longer hold the proxy request indefinitely, and continuously active long responses still have no total-duration cap.
