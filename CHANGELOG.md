# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.8] — 2026-08-15

### Added

- First-party DeepSeek integration for exact `api.deepseek.com` routes, with protocol capabilities driven by endpoint and actual wire format instead of a model-ID allowlist. Current and future models receive the same documented search, effort, transport, and usage handling; native same-protocol requests retain unknown fields, cross-protocol bridges map fields supported by their target API, future effort values pass through, and model capacity is never hard-coded.
- Native DeepSeek V4 Responses support for hosted Web Search, Grok Build function tools, passthrough of the documented custom `apply_patch` shape when explicitly supplied, terminal completed/incomplete/failed events, protocol-appropriate Bearer or `X-Api-Key` authentication, and current reasoning-effort normalization. Current Grok Build source uses the function form for local `apply_patch` execution.
- DeepSeek structured-output adaptation: Responses keeps native `text.format` JSON Schema, Chat converts Grok Build schemas to documented `json_object` mode plus a schema instruction for local validation, and Messages continues through Grok Build's validated `StructuredOutput` tool.

### Changed

- A proxied custom model selected through `[models].web_search` or `GROK_WEB_SEARCH_MODEL` is projected as backend-search enabled for the entire proxy lifetime, including ordinary Grok Build sessions and its dedicated WebSearchClient request, across Responses, Messages, and Chat provider formats. Selection overrides an explicit false only in the temporary proxy configuration, performs no startup probe, and is restored exactly on stop.
- Models on the exact first-party `api.deepseek.com` endpoint enable provider-hosted search when `supports_backend_search` is omitted. Responses and Messages remain on their configured native APIs, while Chat defaults to a Responses bridge and honors an explicit dialect. Explicit false opts out unless that route is selected as the default search model, and relays receive no first-party assumptions.
- Upstream idle protection now covers every response body, including non-streaming success and error JSON. Ordinary channels use a 601-second header/body idle limit, one second beyond Grok Build's 600-second shell default; first-party `api.deepseek.com` uses 660 seconds so empty-line and SSE keep-alives can cover the provider's documented ten-minute queue without imposing a total request deadline.
- Configuration rewrite recovery uses one strict named format and records provider-inherited maximum-completion projections alongside temporary protocol, search, reasoning, feature, and route fields. Unknown state formats are rejected instead of being interpreted by the current schema.
- First-party DeepSeek models expose the documented `None`, `Low`, `High`, and `Max` menu even when Grok Build resolves the wire protocol from its catalog; explicit user menus still win. Messages turns Grok Build's omitted `None` into `thinking.type=disabled` and sends only `output_config.effort`. All three protocols follow the current official effort mapping (`minimal`/`low` to `low`, `medium`/`high`/`xhigh` to `high`, and `max` to `max`; `minimal` is Responses-only), and every format encodes disabled thinking natively. Chat documents the provider's separate `/beta` requirement for strict function schemas. Explicit Responses user-isolation IDs survive Chat and Messages protocol bridges.

### Fixed

- Backend-search declarations now have explicit cross-format failure coverage: every provider protocol receives its native search shape, unsupported upstream HTTP errors remain visible, and a selected search model that silently ignores the hosted tool returns a non-retryable error naming the missing backend `web_search` execution. Grok Build local tools, including arbitrary MCP names, retain declaration, selection, call, and replay semantics through every bridge.
- Missing or untrustworthy provider usage is no longer converted into a valid-looking all-zero measurement. Provider `total_tokens` is preserved only alongside complete input/output accounting, a missing total is derived only from that complete pair, and otherwise `usage: null` lets Grok Build preserve its token baseline instead of resetting auto-compaction progress to zero or recording invented billing values. First-party DeepSeek Responses terminal output now carries complete `usage.context_details`, which current Grok Build uses as its live-context input for auto-compaction.
- Messages and Chat translations now preserve authoritative totals, cache and reasoning token details, and terminal streaming usage blocks while rejecting partial, negative, fractional, overflowing, or all-zero placeholder counts.
- DeepSeek Chat compatibility now preserves tool-call `reasoning_content`, supplies non-null assistant tool content, maps `max_completion_tokens` to `max_tokens` unless explicitly overridden, maps developer messages to system, and requests streaming usage. Because the provider rejects `tool_choice` in thinking mode, hellogrok removes that selector while retaining function declarations for automatic use; explicit non-thinking requests keep the documented selector forms. DeepSeek non-streaming queue newlines and streaming `: keep-alive` comments both renew the dedicated idle window.
- DeepSeek Responses no longer receives a proxy-invented `include` entry that its current API explicitly ignores. Chat `insufficient_system_resource` termination is surfaced as a structured failure or retryable `503`, depending on whether the response protocol has already started.
- Model responses now preserve explicit per-model or provider-inherited `context_window` and `max_completion_tokens` values and protect those selected tiers from conflicting upstream metadata. Provider-only completion limits are temporarily projected because current Grok Build does not inherit them; omitted capacity and search fields remain catalog/metadata-owned. Rolling aliases and future model IDs can therefore change limits and declared capabilities without a proxy release.
- DeepSeek Messages now targets the canonical `/anthropic/v1/messages` endpoint and emits only `X-Api-Key`, preventing both a missing `/v1` path segment and conflicting Bearer credentials.
- Cache-prefix regression coverage now spans GPT, Claude, Grok, Gemini, current and future DeepSeek model IDs, and generic relays across native Responses, Messages, Chat Completions, protocol bridges, parallel tool history, reasoning history, and Grok Build conversation-ID forwarding.

## [0.1.7] — 2026-08-10

### Fixed

- Stopping or quitting now three-way merges proxy-managed configuration fields, preserving edits such as `supports_backend_search`, channel URLs, feature flags, subagent settings, and deleted model channels while restoring untouched temporary values.
- Shutdown now validates the merged configuration and keeps the proxy active if a renamed or moved model would retain a temporary route from the current takeover.

## [0.1.6] — 2026-08-10

### Changed

- The local Windows build helper now leaves `dist/` with only `hellogrok.exe` and `hellogrok-cli.exe`; tagged GitHub releases continue to publish platform-specific artifacts and their verification files.

### Fixed

- Responses hosted search now requests complete `web_search_call.action.sources` metadata without replacing or duplicating existing `include` entries.
- Responses-level citations and Chat `annotations`, `citations`, `search_results`, and `web_search_results` containers are normalized in streaming and non-streaming output to both Grok Build source paths, allowing hosted and configured client search across all three supported protocols to report native deduplicated site counts.

## [0.1.5] — 2026-08-10

### Added

- Channel-scoped endpoints for every Grok Build backend: `/responses`, `/messages`, and `/chat/completions`, backed by one capability-aware protocol facade.
- Protocol-specific tool-history validation for Responses `function_call` pairs, immediately adjacent Messages `tool_use`/`tool_result` batches, and Chat Completions tool calls/results. Invalid history returns a deterministic non-retryable `400` before reaching the provider.
- `chat_search_dialect` channel setting for selecting Chat `web_search_options`, Chat `search_parameters`, or an explicit Messages/Responses search bridge.
- A 180-second response-header wait and SSE idle timeout, without a total request deadline. Any upstream data, including heartbeats, renews the idle window.
- Raw upstream response-model observation for Responses, Messages, and Chat Completions, with terminal declaration precedence and mismatch/conflict diagnostics that do not affect routing.

### Changed

- Channels with `supports_backend_search = true` are exposed to Grok Build as Responses while retaining their real upstream protocol. Responses passes through, Messages receives `web_search_20250305`, and Chat uses its configured hosted-search dialect or protocol bridge; every route returns canonical `web_search_call` items, citations, and source counts to Build.
- Channels with `supports_backend_search = false` keep their configured Grok Build consumer and use client `web_search`, resolved from `[models].web_search`, `GROK_WEB_SEARCH_MODEL`, or Build's authenticated official fallback. The fixed non-streaming WebSearchClient request can use a selected Responses, Messages, or Chat channel through the same facade.
- Configuration rewrite state records the provider protocol separately from the protocol projected to Grok Build, preserves the effective search capability, and restores only the current state format.
- Client-search aliases are rewritten only in protocol-defined tool declarations, choices, and call-name fields; tool arguments, results, response text, URLs, and other business JSON are never traversed.
- A provider that ignores `stream=true` now falls back to buffered SSE in the protocol Grok Build is consuming, including canonical Responses events for capability-projected Messages and Chat channels.
- Channel-owned HTTP headers are parsed as strings and validated before routes are activated; authentication headers remain configurable while proxy-controlled request headers are reserved.

### Fixed

- SSE frames with a private heartbeat event name and an empty `data:` payload are recognized before empty-payload handling and normalized to `: keepalive` comments.
- Native Messages history is rejected locally when an assistant `tool_use` batch is not resolved by leading `tool_result` blocks in the immediately following user message, preventing provider errors such as "tool_use ids were found without tool_result blocks immediately after" from entering repeated retries.
- Messages `thinking` block starts from compatible relays now receive a missing empty `signature` field while later `signature_delta` values remain unchanged, preventing Grok Build's native decoder from failing before the real signature arrives.
- Client-search adaptation now rejects upstreams that return ordinary answer text without independent evidence of a completed search, preventing ignored Chat or Messages search extensions from being reported as successful WebSearchClient results.
- Official DeepSeek Messages channels use `/anthropic/v1/messages` with `X-Api-Key`; official DeepSeek and xAI Chat hosted search uses Responses. Search calls, results, citations, and deduplicated sites are converted back without exposing provider credentials.
- Parallel Responses function calls are converted into one assistant `tool_use` batch followed by one adjacent user `tool_result` batch, preventing valid concurrent tool history from becoming an invalid Messages sequence.
- Configuration restore now preserves a model channel deleted while the proxy is active and still restores every remaining managed field; edits to individual proxy-owned fields continue to fail closed.
- A stalled upstream now returns retryable `504` before response headers or emits the receiving protocol's stream error after streaming begins, instead of leaving Grok Build waiting indefinitely.

### Removed

- The process-local provider search-replay cache. Responses search history is reconstructed statelessly from each `web_search_call` query and source list, with the DeepSeek `action.queries` repair retained where required.

## [0.1.4] — 2026-08-08

### Fixed

- Provider-private `keepalive`, `keep-alive`, `keep_alive`, `heartbeat`, and `ping` SSE frames are normalized to standards-compatible comments before Grok Build's strict Responses event deserializer sees them. Detection covers SSE `event:` names, JSON `type` and `event` fields, raw data payloads, and their `response.*` forms without consuming sequence numbers.
- Responses streams now stop reading after `response.completed`, `response.incomplete`, or `response.failed`; Messages and Chat Completions conversions stop after `message_stop` and `[DONE]`. Closing the response body cancels providers that leave an already completed stream socket open.
- Heartbeat handling and terminal-driven closure now cover all three supported `api_backend` values, including relays that send heartbeats only through the SSE event name.

### Changed

- Stream completion logs include a heartbeat count without recording heartbeat payloads.

## [0.1.3] — 2026-08-08

### Added

- Shared-leader ACP hot reload after proxy enable and disable, including forward and reverse model-ID selection for idle custom-model sessions.
- Safe migration from a uniquely truncated legacy dotted model ID to its normalized full ID.
- Compatibility with both current `session/setModel` and legacy `session/set_model` ACP method names.
- Windows status/log toolbar with usage-day retention choices and next-match log search.
- Log retention by distinct days the application was actually used; the default keeps the latest 7 usage days.

### Fixed

- Model hot switching now preserves ordinary conversation and tool history while removing only provider-encrypted reasoning known to originate from a different channel, protocol, wire model, or upstream endpoint. Legacy opaque state remains pass-through unless the upstream returns a structured signature or decryption rejection, which triggers exactly one clean replay and then a non-retryable error if rejection persists.
- Unquoted dotted model tables are normalized while the proxy is active and restored byte-for-byte on stop.
- Channel IDs, display names, and upstream model names containing dots or dashes retain channel authentication and routing.
- Live Windows named-pipe leaders misreported as stale by Grok Build 1.0.0 are recovered only when their lock is actively held; leftover lock files remain ignored.
- Windows port conflicts now recognize WinSock error 10048 and show a direct `127.0.0.1:18787`-occupied error without touching Grok configuration.
- The legacy singular `api_backend = "message"` spelling is accepted as a compatibility alias for Grok Build's official `messages` backend.
- Disabling the proxy switches normalized dotted-model sessions back to the restored legacy ID; external provider replacement reloads the catalog without selecting obsolete models.
- Client search no longer infers mandatory tool use from prompt keywords, and wire aliases are restored only in structured tool-name fields.
- Provider-only search replay state is isolated by channel, preceding conversation, and stable search identity; ambiguous matches are rejected and Messages search blocks are stored atomically.
- Malformed 2xx Responses, Messages, and Chat Completions envelopes are rejected with 502 instead of being forwarded as successful responses.
- Responses validation now accepts Grok Build's official reasoning items when their optional `content` field is absent or null.
- Deterministic local proxy failures now return `X-Should-Retry: false`, preventing Grok Build from retrying configuration and schema errors up to its full retry budget; upstream transport failures remain retryable.
- Successful HTML pages from a misconfigured upstream URL are rejected with a direct `base_url`/`api_backend` diagnostic instead of a raw JSON parse error.
- Anthropic Messages and Chat Completions requests now preserve `stream=true`; their upstream SSE is translated incrementally into Responses events for reasoning, text, function arguments, hosted search, and terminal errors instead of being buffered and replayed as fake streaming.
- Search URLs from Responses output, Messages results, and Chat citations are emitted in both `web_search_call.action.sources` and `output_text.annotations`. Any of the three protocols can therefore serve as Grok Build's selected client-search model independently of `supports_backend_search`, while hosted search uses the same normalized output. Both render paths can show the native deduplicated site count. When search execution is independently confirmed but structured citations are missing, valid HTTP(S) links in the final answer are used as source evidence; ordinary answer links never create a search call, and no count is invented without a real URL.
- An upstream that ignores `stream=true` and returns JSON remains usable through a buffered SSE fallback, and that downgrade is stated explicitly in the proxy log.

### Changed

- Active or input-blocked sessions are skipped during hot reload; `--no-leader` sessions report the manual `/model` fallback instead of being treated as refreshed.
- Proxy startup and shutdown keep a static tray label; the item is temporarily disabled and failures are shown in a dialog.
- Logs append across proxy sessions instead of being truncated, and the status panel is grouped into scan-friendly sections.
- Proxy startup no longer probes upstream search capabilities. Omitted and false `supports_backend_search` values use client search, explicit true values are trusted, and an explicitly selected search model is routed without startup validation.
- Upstream authentication now defaults to Bearer for every backend, matching Grok Build; Messages providers that require `X-Api-Key` must opt in with `auth_scheme = "x_api_key"`.

### Security

- Opaque reasoning provenance survives proxy restarts in a bounded private index containing only SHA-256 digests; raw reasoning signatures, channel IDs, model names, upstream URLs, and credentials are never persisted.

## [0.1.1] — 2026-08-07

### Added

- Explicit search model selection via `[models].web_search` config key and `GROK_WEB_SEARCH_MODEL` environment variable, with env taking precedence over config.
- CC Switch takeover detection before config rewrite — start is refused when CC Switch already owns Grok Build.
- Single-instance enforcement for both tray and foreground modes via OS-level lock.
- Windows log window application icon display in title bar and taskbar.
- Configured search route resolution logging at startup, visible in `hellogrok routes` and the log window.

### Changed

- Tray now defaults to proxy-enabled on first launch.
- Tray quit defers exit when a config-ownership conflict exists, preventing orphaned proxy URLs.
- SIGINT/SIGTERM handlers retry stop on deferred errors instead of leaving the process inconsistent.
- Tool choice normalization respects the configured `supports_backend_search` value.

### Fixed

- Managed search flag now placed after channel settings in config rewrites, preventing key-ordering issues.
- Client search wire alias properly hidden from upstream requests.

## [0.1.0] — 2026-08-07

### Added

- Initial release: cross-platform local proxy for Grok Build custom model channels.
- Response normalization for `responses`, `chat_completions`, and Anthropic-compatible `messages` APIs.
- Native Web tool support: `web_search` (hosted and client modes) and `web_fetch`.
- Channel-owned authentication isolation (API keys, env keys, auth providers, headers).
- Windows native tray application with proxy toggle, autostart, status/log window, and quit.
- CLI for foreground proxy, route inspection, config restore, autostart management, and log viewing.
- Automatic configuration preparation and recovery on normal and abnormal exit.
- Login autostart for Windows (registry), Linux (systemd user service), and macOS (LaunchAgent).
- CC Switch compatibility detection and conflict warnings.
- Builds for Windows, Linux, and macOS on amd64 and arm64.

[Unreleased]: https://github.com/hellowind777/hellogrok/compare/v0.1.8...HEAD
[0.1.8]: https://github.com/hellowind777/hellogrok/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/hellowind777/hellogrok/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/hellowind777/hellogrok/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/hellowind777/hellogrok/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/hellowind777/hellogrok/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/hellowind777/hellogrok/compare/v0.1.1...v0.1.3
[0.1.1]: https://github.com/hellowind777/hellogrok/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/hellowind777/hellogrok/releases/tag/v0.1.0
