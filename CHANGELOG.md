# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.39] — 2026-09-16

### Added

- Managed `inference_idle_timeout_secs` for proxied channels: when a custom model does not configure Grok Build's per-chunk idle deadline, hellogrok temporarily materializes 1800 seconds (restored byte-for-byte on stop) so long relay stalls — queued generations that emit heartbeats but no content for many minutes — no longer hit the 600-second shell default. Explicit per-model or global `[models]` values remain user-owned and are never rewritten; first-party `api.deepseek.com` routes are excluded so their remote metadata stays authoritative (the watchdog below still protects them).
- Downstream content watchdog on every streaming path (native Chat/Messages/Responses passthrough and the translated Responses projections). It mirrors Grok Build's per-protocol "meaningful content" idle classification — text, reasoning, tool-call deltas, and terminal signals count; keepalive comments, usage-only chunks, empty deltas, and lifecycle bookkeeping events do not — and fires one 30-second margin before the client's own content-progress timer. On fire it closes the upstream body and emits a retryable `proxy_stream_error`, so a genuinely stalled stream re-enters Grok Build's native 15-attempt retry budget instead of dying on the client's non-retryable `IdleTimeout`. Watchdog activity is logged as `SSE stalled: no content reached Grok Build for …`; timeouts below 60 seconds disable the watchdog to respect explicit fast-fail choices.

### Fixed

- A relay holding a stream with only keepalive bytes for longer than Grok Build's idle deadline (observed: GLM channels queuing for 3+ minutes with heartbeat-only frames) previously killed the turn permanently: the sampler's content-progress timer raises `IdleTimeout`, which is classified non-retryable, so neither the 15-attempt budget nor error-triggered compaction could save the turn. The turn now waits out stalls up to the managed deadline and degrades to a normal retryable failure past it.

## [0.1.38] — 2026-09-16

### Added

- Usage rectifier that repairs provider token-usage measurements before they reach Grok Build's token ledger and auto-compaction meter. Stringly-typed counts (`"prompt_tokens": "12345"`), whole floats, and `json.Number` values are normalized to integers; invalid optional decorations (null detail values, non-map detail containers, fractional `cost_in_usd_ticks`) are removed without poisoning the core measurement; unrecoverable core counts (negative, fractional, overflowing, non-numeric) are deleted so the downstream validator drops the measurement instead of trusting corrupt data. Applied across Chat Completions, Responses, and Messages, in streaming and non-streaming responses, and in protocol-translation paths, with repairs logged as `usage rectified` proxy notes.
- `streamFrameRectifier` interface unifying the three frame-rewriting stream rectifiers (Chat tool calls, Messages tool blocks, Responses function calls) under a single contract, so future protocol rectifiers plug into the same pipeline without touching dispatch logic.

### Fixed

- Channels whose relays report usage with non-standard JSON types (GLM and potentially other one-api/new-api-style gateways) no longer have their entire usage measurement silently dropped on format defects. Grok Build now receives real provider token counts instead of falling back to a byte-based estimate that overshoots by 25% or more on Chinese and JSON-heavy content, which previously fired auto-compaction at roughly 50% real usage instead of the configured threshold. The premature compaction dropped all tool results, the model re-read files it had already processed, context ballooned again, and a second compaction stalled the session entirely.

## [0.1.37] — 2026-09-16

### Fixed

- Quoted think tags are prose, not span delimiters: a `<think>` or `</think>` occurrence wrapped in backticks or double quotes no longer opens or closes an inline think span. Reasoning that discusses the tags keeps its full span in the reasoning channel instead of terminating early and leaking the remainder into the visible reply, and tags quoted inside visible text reach the client intact; only unquoted stray closing tags are still removed.
- The non-streaming content path classifies unbalanced closing-tag reasoning tails and removes stray closing tags only on complete message objects (those carrying a `role`); streaming deltas leave classification to the turn-level state machine, where a delta-level strip previously consumed a closing tag before the state machine saw it.

## [0.1.36] — 2026-09-16

### Added

- Vision-rejection recovery for text-only channels: a `400` whose error code contains `vision` or whose message contains `not multimodal` / `does not support image` triggers one rewrite-and-retry in which every image content part (`image_url`, `input_image`, `image`, including images inside tool results) is replaced by a text placeholder stating that the visual payload was omitted. Applied on all three upstream protocols; channels are not remembered as text-only, so each image-bearing request re-derives the decision from the upstream rejection.
- Parameter-alias normalization against the tools declared on the request: `target_path` now maps to `target_file`, `target_directory`, or `file_path`, whichever the declared schema actually advertises, joining the existing alias table in the shared tool-compatibility layer. Rewrites apply only to properties the tool declares.
- Discard of argument-less streamed tool calls before Grok Build dispatch: a call that accumulates no argument fragments at all is dropped when the resolved declared tool requires properties (an empty arguments object can never satisfy them), reported as an `empty-args-discarded(name=…)` proxy log note. Tools without required properties keep their empty arguments, the valid zero-argument convention.

### Fixed

- Inline reasoning no longer leaks into the visible reply. The Chat think-tag state machine strips `<think>…</think>` spans wherever they appear in the stream instead of only at the head of the answer: text before an opening tag is emitted immediately, the span is buffered until its closing tag and routed to the reasoning channel, an unbalanced closing tag at the head of a turn classifies the preceding text as reasoning, and stray or delta-split closing tags are removed or held instead of being shown. The non-streaming content path peels every span from the answer. Reasoning arriving after visible reply text remains dropped by design.
- Third-party models that emit `target_path` for Grok Build's path parameters no longer surface `Failed to parse arguments for tool …: missing field …`; relay or model glitches that emit a tool call with no argument fragments no longer burn a guaranteed-failure dispatch round.

## [0.1.35] — 2026-09-15

### Added

- Head-truncation repair for tool calls whose first stream delta was lost in transit: arguments missing their opening `{"` are restored when the prefixed payload parses as exactly one complete JSON object, and empty function names are inferred from the argument key set against the tools declared on the request (the shared `command`+`description` shape resolves deterministically to `run_terminal_command`; remaining ties stay unrepaired). Applied in the shared tool-compatibility layer across `chat_completions`, `messages`, and `responses`, streaming and non-streaming, and on persisted history replayed in later requests, with the repaired name backfilled onto the matching tool-result message.
- Stream-terminal emission of Chat tool frames: the chat rectifier holds tool frames until `[DONE]`, stream end, or an error frame instead of flushing at `finish_reason`, so argument fragments a relay emits after `finish_reason` merge into complete arguments instead of being discarded tail-truncated. Held inline reasoning still flushes at `finish_reason`.
- Hold rectifiers for name-less tool blocks: Messages `tool_use` blocks are held between `content_block_start` and `content_block_stop` and re-emitted as start (resolved name) + one complete `input_json_delta` + stop; Responses `function_call` items are held between `response.output_item.added` and `response.output_item.done`, with the done frame's complete item as a second source for name and arguments. Unclosed blocks and items flush at their protocol terminals.
- `late-tool-deltas-discarded(index=N)` proxy log note for tool-call deltas discarded after an early flush, making relays that emit `finish_reason` before their last argument fragments diagnosable.

### Fixed

- Third-party relays that drop a tool call's first streamed delta no longer surface as `Agent tried calling a tool that doesn't exist` with head-truncated arguments; the damage is repaired before Grok Build dispatch, and broken records persisted in earlier turns are repaired on replay instead of reaching the upstream verbatim.

## [0.1.34] — 2026-09-14

### Added

- Responses absorb window for retryable `response.failed` SSE terminals. While nothing has been written to the client, a Responses stream whose only terminal is a retryable failure (rate limit, concurrency, overload) is withheld — response headers and early frames stay buffered up to 32 frames — and replayed inside the absorb window with the same backoff and `Retry-After` handling as HTTP soft failures. Grok Build keeps its full retry budget; an exhausted window passes the failure through retryable. `absorb_retry_max_secs = 0` streams the failed event immediately; deterministic `response.failed` events pass through without retry.
- Structured upstream error summaries for HTTP errors and Responses `response.failed`/`error` events (`type`, `code`, `message`), with bearer tokens, key assignments, and `sk-` values redacted and long messages truncated. An official Grok catalog name (for example `grok-4.6`) observed on a non-xAI custom channel is logged with body size, tool count, and session presence to aid `/resume` diagnosis; first-party `api.x.ai` routes and custom IDs without the `grok-` prefix are excluded.
- Explicit non-retryable `502` responses for stream-shape mismatches: an SSE body answered to a non-streaming request, or a streaming body answered to Grok Build's fixed non-streaming WebSearchClient request.

### Fixed

- Concurrency-limit rejections (`concurrency`, `concurrency limit`, and the corresponding Chinese phrases) classify as transient, and error envelopes nested under `response.error` are recognized the same as top-level `error` objects.
- Client disconnects no longer surface as upstream stream failures. Messages, Chat Completions, native, and Responses streams log a client abort and emit no `proxy_stream_error` when the client write fails or the request context is canceled; truly truncated upstream streams still emit the error.
- Benign upstream-model mismatches log once per channel/protocol/configured/upstream pair (bounded to 256 keys); conflicting and invalid declarations always log. Responses SSE headers are sent only after the hold decision, so an absorbed retry never emits partial headers.

## [0.1.33] — 2026-09-14

### Fixed

- Streaming terminal detection is protocol-generic and does not key off a channel, provider, or model name. A relay that produces a protocol stop signal (Chat `finish_reason`, Messages `stop_reason` / `message_stop`, Responses snapshot `status` completed/failed/incomplete or collected output items) then closes without the wire trailer Grok Build wants (`[DONE]`, `message_stop`, `response.completed`) is completed in-place: hellogrok synthesizes the missing trailer instead of injecting `proxy_stream_error`. That false error previously made Grok Build retry an already-finished turn up to 15 times and surface `Server error: Something went wrong on our side`. Chat finish chunks that omit `delta` get an empty object so the decoder accepts them. Truly truncated streams (no stop signal and no output items), idle timeouts, and read failures still emit `proxy_stream_error`.

## [0.1.32] — 2026-09-14

### Changed

- Removed the implicit `supports_backend_search = true` default for models on the exact first-party `api.deepseek.com` endpoint. DeepSeek has retired its hosted web search — the current Responses API documents `web_search` and all other built-in tools as ignored — so the old default routed `web_search` to a channel that silently answers without search results, bypassing the client-search fallback chain (`[models].web_search`, `GROK_WEB_SEARCH_MODEL`, authenticated official fallback) with no error or sources. An omitted field now preserves Grok Build catalog behavior for every provider uniformly; explicit `true`/`false` values keep their meaning, and a channel explicitly set to `true` still works for relayed endpoints that implement a real search extension. DeepSeek-specific protocol handling (the `[1m]` Messages alias, thinking normalization, 660-second idle policy, and the Chat Responses bridge dialect) is unchanged.

## [0.1.31] — 2026-09-13

### Fixed

- Provider usage reports that contradict the live request context no longer inflate Grok Build's context meter or fire auto-compact above the configured threshold. Some relays double-count the cached prefix — or report session-cumulative prompt tokens — in a single response, and Grok Build feeds the reported total straight into its meter and `auto_compact_threshold_percent` check, so one bad report made a session at roughly half of its true context read as 91% full and triggered a minutes-long compaction that was never needed. hellogrok now learns a bytes-per-token ratio per conversation (keyed by channel plus the client's session identity) from the first coherent report and suppresses later reports whose prompt count overshoots what the tokenizable request body can hold; embedded base64 data URIs are excluded from that footprint because they carry image bytes at a nearly constant token cost. A suppressed report is dropped entirely, leaving Grok Build on its own byte-based estimate until the provider reports coherently again. Context rewrites (compaction, rewind, resume) relearn the baseline, conversations without a stable session identity skip the check, and billing-visible behavior for coherent providers is unchanged.

## [0.1.30] — 2026-09-13

### Added

- Closed-Responses-schema negotiation for hosted web search. The standard `web_search_call.action.sources` include hint remains the default for compliant providers. Volcengine Ark (`*.volces.com`) — whose `include` is a closed enum rejecting unknown values with `InvalidParameter`, and whose tool schema rejects unknown declaration fields such as `filters` and the `allowed_tools` selector — is handled ahead of time: the hint is omitted and the hosted-search declaration is sanitized to the documented field set, with a pure hosted-search `allowed_tools` selection collapsed to `required`. Any other upstream that proves its schema closed with a `400` (include or tool-declaration rejection) triggers one in-request rewrite-and-replay; the rewrite is single-shot per request and no per-host, per-channel, or on-disk state is retained. A rejected request never executed a search, so the replay is side-effect free and consumes no billed search quota.

### Fixed

- Hosted-search responses whose `url_citation` annotations omit `start_index`/`end_index` no longer fail Grok Build's annotation parser: missing indices are filled with `0` on every route, in non-streaming bodies and streaming terminal events alike, preserving search availability at the cost of citation highlight precision only.

## [0.1.29] — 2026-09-13

### Added

- Global `[models]` `error_resilience` setting (`off` default, `balanced`) controlling deterministic 4xx failures. Soft-failure absorption (retryable `5xx`, `429`, upstream `408` edge-timeout pages, Cloudflare origin-TLS `525`/`526`, response-header timeouts) is always on: those failures clear on their own and Grok Build's retry of them only burns time. By default, deterministic 4xx rejections (authentication, permission, invalid request/model) pass straight through with the provider's explanation and `X-Should-Retry: false`, because retrying them cannot succeed. The opt-in `balanced` value additionally absorbs them inside the absorb window with a fixed 30s-then-60s wait sequence, so unattended turns survive relay-side token rotation, permission fixes, config reloads, and brief deploys. A reasoning-history rejection is never absorbed, because it reports a foreign conversation state the user must see. Once any absorb window is exhausted, the failure passes through retryable and Grok Build's own retry indicator shows the error reason (`<headline> | Retrying (N/M)`) while its native budget continues — the failure never hides behind a bare retrying state.

### Fixed

- An upstream `408` no longer kills the turn. Between two servers a 408 is an edge/gateway timeout page, not the RFC client timeout Grok Build's terminal classification assumes; hellogrok now retries it inside the absorb window and remaps an exhausted one to a retryable `504`, so Grok Build keeps its native retry budget instead of failing on first sight.
- The dead-channel breaker now counts every TLS handshake failure form Go actually produces — remote/local alert `net.OpError`s, `tls.RecordHeaderError`, and the HTTPS-to-plain-HTTP scheme mismatch — instead of only certificate-verification failures, so a channel with broken TLS termination fast-fails instead of burning the full client retry budget when `dead_channel_fail_fast` is enabled.
- Requests caught mid-flight by a proxy stop now receive the structured `503 proxy_stopped` diagnostic instead of a one-off retryable `502 upstream: context canceled`, so stale sessions get the same "reselect the model" signal as fresh requests.
- Invalid-TOML config recovery no longer deletes a user-edited root `subagents.enabled` dotted line. The line-scoped fallback path now drops the line's created marker when the value no longer matches what hellogrok applied, matching the parse path's protection.
- Responses streams no longer leak post-answer reasoning through the `response.output_item.done` frame: the event now honors the same drop/strip rules as its `added` counterpart (non-encrypted post-answer reasoning is dropped; encrypted items pass through with visible text stripped). `reasoning_summary_part.added/done` text now goes through the protocol self-talk filter, and a post-answer `</think>`-only content part is dropped.
- The dead-channel breaker's TLS classification, the Status-and-logs window debounce (a build slower than the 5-second timeout could spawn a second window), and leader-list deduplication (multiple reachable leaders without a socket path were collapsed into one) are corrected.
- Chat history reasoning replay is no longer triggered by a user-chosen channel ID that merely contains "deepseek"/"mimo"; the heuristic now keys on the wire model and endpoint evidence only.
- `scripts/build.ps1` now aborts when `go build` fails instead of printing `OK` for a stale binary.
- A passthrough error that outlasted a long absorb window now tells the user how stale it is instead of masquerading as a live failure: hellogrok stamps `X-Hellogrok-Absorb-Delay` on the response and prefixes structured JSON error messages with a one-line `[hellogrok: upstream stayed failing for ...]` note. Windows shorter than 5 seconds stay invisible.
- The invalid-TOML config fallback now also drops management of feature-flag lines whose entire `[features]` section the user deleted, matching the parse path instead of acting on lines that no longer exist. A new invariant test (`TestPrepareRestorePathsAgree`) pins the two restore paths to the same managed-state decision for the same input, and the package header records the two-path modification discipline.
- The Responses thought gate is now table-driven with a fail-loud default: every gated event type has exactly one registered rule, and an unregistered event that visibly carries reasoning is intercepted and logged instead of leaking a second Thought under the reply. Behavior for all previously known event types is unchanged. Items and content parts whose type names reasoning in another dialect (`thinking`, `reasoning_summary`, `redacted_thinking` — what relays that bridge Anthropic-style blocks through protocol conversion emit) pass through the same gate as `reasoning` instead of leaking ungated, while unrelated unknown events keep passing untouched. The terminal `response.completed` frame applies the same sanitize and post-answer clearing to those variants as the streamed frames, and the fail-loud interception log deduplicates chatty unregistered delta types.
- A reasoning-history rejection that a relay wraps in an HTTP 200 error envelope now vetoes the absorb layer exactly like its plain-error counterpart, instead of burning the whole window in the balanced tier on a request that has no recovery rewrite.

## [0.1.28] — 2026-09-12

### Fixed

- Relays that answer a failure with HTTP 200 plus an error envelope no longer die as an opaque envelope-validation rejection. A transient envelope (rate limit, overloaded, timeout) now enters the absorb window and is retried inside the proxy; a deterministic envelope (authentication, billing, invalid request) passes through with the provider's explanation and a terminal disposition. Responses terminal bodies that carry their own error member keep the native failed-response path.
- A Cloudflare shield challenge in front of a relay (`cf-mitigated: challenge` or a Cloudflare-served challenge page) is treated as transient: absorbed inside the retry window, and passed through as a retryable `503` when it outlasts the window, because a bare `403` classifies terminal in Grok Build. A genuine origin `403` without Cloudflare markers is untouched.
- Non-streaming Chat Completions responses whose relay reuses one `tool_call` ID across calls are remapped to fresh unique IDs, matching the streaming rectifier's tolerance, instead of being rejected; the next round's tool history stays valid.
- Responses bodies that omit envelope bookkeeping (`id`, `object`, `status`) on an otherwise complete terminal payload get the missing markers synthesized instead of a 502 rejection; present-but-wrong values and a missing or malformed `output` remain rejections.

### Added

- Regression coverage for wrapped-2xx absorb-then-recover and deterministic passthrough, Cloudflare challenge absorb/passthrough and origin-403 separation, envelope bookkeeping synthesis, and non-streaming duplicate call-ID remapping.

## [0.1.27] — 2026-09-12

### Fixed

- Cloudflare origin-TLS failures from relays (`525` handshake failed, `526` invalid origin certificate) are now retried inside the absorb window before reaching Grok Build, which classifies both statuses as terminal and fails the turn on first sight. The request never reached the relay's origin application, so the proxy-side replay is side-effect free; a relay origin restart or certificate rotation that clears inside the window no longer interrupts a turn, and an exhausted window passes the failure through with the terminal disposition Grok Build expects.
- The passthrough retry disposition now mirrors Grok Build's edge-client policy (`429` and every `5xx` except `525`/`526` are retryable). The previous narrower status list stamped `X-Should-Retry: false` on transient Cloudflare edge pages (`520`–`524`, `529`, `530`), and that header vetoes Grok Build's own retry, turning a self-clearing edge blip into an instant turn failure.

### Added

- Regression coverage for origin-TLS absorb-then-recover, terminal passthrough after the absorb window, and the retryable disposition of Cloudflare edge pages.

## [0.1.26] — 2026-09-12

### Fixed

- A response-header timeout no longer cancels the context shared by the retry loop. Previously the attempt timer canceled that shared context, the absorb layer's wait returned immediately, and the handler finished without writing anything: the client received an empty `200` instead of the designed retryable `504`, and absorb retries after a header timeout were a dead path. The timeout now cancels only the failing attempt's derived context, so a header timeout produces the retryable `504`, the absorb layer retries within its configured budget, and a successful retry surfaces as a normal response. Regression tests cover both paths.
- An opt-in dead-channel breaker probe that ended without reporting an outcome (the caller disconnected mid-probe, or the probe's response headers timed out) previously left the breaker latched in probing state, fast-failing the channel permanently even after connectivity returned. Each probe now carries a 2-minute lease; an expired lease releases a fresh probe on the next request. Probes that report an outcome close or re-arm the breaker as before.
- Proxy apply and restore now re-read `config.toml` immediately before the atomic rename. When another program changed the file after hellogrok read it for the current operation, the write aborts with a retryable error before touching the file, preserving the concurrent edit; the rewrite state remains on disk for a simple retry.
- Recovery removes a hellogrok-created `subagents.enabled` dotted key only from the configuration root table. The user's own `subagents.enabled` inside any other table (for example `[model.one]`) is no longer deleted as a temporary projection.
- When `config.toml` no longer exists, `hellogrok restore` drops the obsolete recovery record instead of failing every later start and restore on a record that can never succeed. An unsupported rewrite-state format error now names the state file to delete.
- A corrupt `capacity_cache.json` or preferences file no longer blocks startup or later reads and writes. The unparseable file is preserved with a `.bad` suffix and the application continues with an empty capacity cache or default preferences; both are derived state that rebuilds from live traffic and the next write.
- On Windows, console allocation failures other than "a console already exists" surface as errors, and a failure to bind `CONOUT$` / `CONIN$` returns an error instead of silently leaving stdio unbound. Repeated **Status and logs** clicks while the window is still being created no longer start a second creation attempt, and the creation-timeout error points to the actual log-file path.

### Changed

- Atomic configuration writes flush the renamed file's directory entry so a power loss cannot lose a committed rename (best-effort on filesystems without directory sync).
- The release workflow fails a tagged build whose tag does not match `Version` in `internal/appinfo/appinfo.go`, and CI pins `govulncheck` to v1.1.4 for reproducible vulnerability scans.

## [0.1.25] — 2026-09-12

### Fixed

- Responses streams no longer fail when an upstream reuses one item ID across output slots (observed on sub2api-style gateways that share a single reasoning ID per response). The colliding later slot is remapped to a fresh unique ID that keeps the provider's type prefix (`rs_`, `ws_`, `msg_`, …), and every later `item_id` event for that slot — reasoning summary deltas, function-call argument deltas, and the final `response.completed` snapshot — is rewritten consistently. Affected turns previously exhausted Grok Build's retry budget and ended as `Server error (500)`.
- Chat Completions streams now assign a unique `call_` ID to any `tool_call` whose upstream ID was already used by another call in the same response, keeping the next round's tool history valid instead of tripping request-side history validation.
- The per-stream remap count is logged (`remapped N duplicate upstream item id(s)`) so a defective upstream remains visible in diagnostics.

### Changed

- Responses stream identity validation distinguishes two failure classes: cross-slot ID reuse (remapped, stream survives) and same-slot identity conflicts (still rejected). Request-side tool-history validation is unchanged.

## [0.1.24] — 2026-09-12

### Fixed

- Native Chat SSE no longer lets a later empty `"name"` overwrite a known tool name, and no longer rewrites incomplete argument JSON per frame. Grok Build's last-write-wins Chat accumulator therefore keeps `list_dir` / `run_terminal_command` instead of reporting NotFound.
- Empty or vendor `finish_reason` values (`""`, GLM `sensitive` / `network_error` / `model_context_window_exceeded`) are dropped or mapped onto Grok Build's Chat enum so serde no longer cancels the stream.
- Reasoning is forwarded only as a prefix sibling. Thought after visible text is dropped on Chat, Messages, and Responses; `<think>…</think>` blocks are peeled out of answer text; protocol self-talk such as `reply only:` / `任务已全部完成` is stripped from prefix CoT. Streaming `reasoning_content` deltas keep leading BPE spaces.
- Chat history no longer replays plaintext CoT across user turns except on DeepSeek and MiMo. Intra-turn tool-loop reasoning is kept. Encrypted or signed blobs are never removed, and `"tool call"` placeholders are never injected.

### Changed

- Ordinary Chat and Messages channels stay on Grok Build's first-party mapper. Responses projection remains search-only.
- Upstream Chat tool lists keep Grok Build `client_name` values. Claude/Codex aliases are rewritten on inbound calls instead of being cloned into every `tools` array.
- Every upstream request is presented as Grok Build (`grok-shell` User-Agent and `X-Grok-Client-Identifier`). hellogrok's own product string is never forwarded.
- Streaming Chat keeps `stream_options.include_usage=true`. GLM Chat with tools also sets `tool_stream=true` when omitted.

## [0.1.23] — 2026-09-11

### Fixed

- Align Grok Build's local capability surface for every custom channel, including relayed grok-4.5/4.6 endpoints that arrive as Responses, Messages, or Chat Completions. Tool-call names and wire shapes are resolved back to the function tools declared on the current request: empty or top-level Chat names, legacy `function_call`, object-valued arguments, Claude/Codex/display aliases, argument-shape recovery, and common XML/JSON-in-content calls.
- Project Claude/Codex aliases (`LS`, `Read`, `Bash`, `Task`, …) onto coding-session tool lists so models can discover Grok Build tools under names they already know, then map those calls back to `list_dir`, `read_file`, `run_terminal_command`, `spawn_subagent`, and the rest before dispatch.
- Wrap direct MCP names (`server__tool`, `mcp__server__tool`) as `use_tool` when that meta-tool is declared. Convert Write-style full-file payloads to `write` or `search_replace`, Glob-style patterns to `glob`/`grep`/`rg --files`, and fill required `description` / `subagent_type` fields that Grok Build schemas reject when omitted.
- Repair request history and tool-result `name` fields so a mismatched alias call cannot poison the next thinking-model turn into a retryable stream error. Exact Grok Build names from relayed grok-4.5/4.6 sessions are left unchanged.

## [0.1.22] — 2026-09-10

### Fixed

- Normalize missing Chat and Messages local tool-call IDs before delivery; repair only unambiguous missing-ID pairs in existing Chat history.
- Preserve Responses output-item and function-call identities across stream events and reject conflicts. Assign tool indexes when converting buffered Chat responses to SSE.
- Extend OpenCode session projection to Go and Zen across all three protocols. Extract identity before conversion and reuse headers for retries; unidentified requests receive isolated operation IDs instead of being blocked locally. Cross-request affinity remains unavailable without client identity.
- Preserve configured or incoming OpenCode User-Agent headers; when absent, derive Grok Build identification from the installed executable instead of advertising hellogrok.

### Added

- Display the running application version in the Windows status/log window title.
- Add opt-in live-provider read-tool verification and regression coverage for tool identity, parallel fallback calls, and OpenCode request headers.

## [0.1.21] — 2026-09-07

### Added

- Internal `SessionIdentity` normalization for Grok Build requests. Stable conversation identity can come from an existing upstream session header, Grok Build conversation/session headers, standard session headers, or `metadata.session_id`.
- OpenCode Go upstream projection: custom routes under `opencode.ai/zen/go` now receive `x-opencode-session` when the incoming request supplies a stable identity. Existing non-empty values remain authoritative, and requests without a stable identity are not assigned a random per-request ID.

### Fixed

- OpenCode Go custom channels no longer lose Grok Build's conversation affinity when hellogrok rebuilds the upstream request for protocol conversion.

## [0.1.20] — 2026-08-30

### Added

- Per-channel absorb layer for transient upstream soft failures. While the upstream response headers have not been sent, retryable failures (busy/overloaded `503`, `429`, retryable `5xx`, response-header timeouts) are retried inside the proxy with exponential backoff (2s–30s, honoring an upstream `Retry-After` up to 60s) for up to 90 seconds by default (`absorb_retry_max_secs`, `absorb_retry_backoff_cap_secs`). Grok Build's native 15-attempt retry budget stays untouched during the window; only when the window is exhausted does the failure pass through, still retryable. Transport errors are not absorbed so Grok Build's first-retry HTTP/1.1 client rebuild keeps working. `UP absorb` log lines record each wait.
- Synthesized 30-second `Retry-After` on retryable busy/overloaded `5xx` passthroughs that carried none, pacing Grok Build's own retries. Upstream `Retry-After` headers are preserved.
- Opt-in dead-channel breaker (`dead_channel_fail_fast`, default off; `dead_channel_fail_threshold`, default 6). Only dial-level failures (connection refused, DNS, TLS handshake) count, because those can never succeed on retry; after the streak the proxy answers a non-retryable `503 proxy_circuit_open` with `X-Should-Retry: false`, with one probe allowed after a 5-minute cooldown and any upstream response closing the breaker.

### Changed

- The circuit breaker no longer fast-fails soft failures. Busy `503`, `429`, timeouts, transport resets, and error-body read failures pass through as retryable (or are absorbed first), so Grok Build keeps its complete native retry budget. `X-Should-Retry: false` is reserved for deterministic failures whose retries cannot succeed (authentication, permission, billing, quota, invalid request/model).

## [0.1.19] — 2026-08-29

### Fixed

- Provider token counts larger than the known `context_window` are no longer forwarded as Grok Build live context. Hosted-search billing totals, oversized `context_details`, and translated Messages/Chat measurements that cannot be the prompt of a successful request become `usage: null`, so Grok keeps the previous baseline instead of compacting on a 1.7M / 1.0M display. Ordinary in-window usage is unchanged.

## [0.1.18] — 2026-08-28

### Added

- Per-channel upstream failure circuit breaker. Grok Build retries retryable statuses up to 15 times (≈5.5 minutes per turn), so a channel whose relay is down leaves the user stuck in the "retrying" phase until every attempt fails. After 4 consecutive retryable upstream failures (5xx, transport errors, or error-body read failures) the proxy now answers immediately with a non-retryable `503 proxy_circuit_open` and `X-Should-Retry: false`, so the turn fails fast instead of burning the retry budget. A single probe request is allowed through after a 90-second cooldown: success closes the breaker automatically, failure re-arms it. Any non-5xx upstream response (including 429) resets the failure streak, and each channel has an independent breaker. Streaming failures that occur after headers are sent are unaffected.
10→
## [0.1.17] — 2026-08-16

### Changed

- Proxied custom channels now use their model-table ID as the Grok Build runtime identity while retaining the configured `model` as the upstream wire model. Normal stop restores the original assignment byte-for-byte, and new or updated session summaries can distinguish channels that share the same provider model.
- An ordinary proxy stop keeps a diagnostic listener so stale sessions receive a structured, non-retryable `proxy_stopped` response. Final application shutdown closes that listener and releases the local port.

### Fixed

- `/resume` no longer falls back to an official model merely because a custom channel uses the same upstream model name; the selected custom channel identity is preserved across Responses, Messages, and Chat Completions.
- Upstream error status and body remain visible while structured retry classification stops authentication, permission, billing, insufficient-balance/quota, invalid-request, and invalid-model failures from entering generic retry loops. Rate-limit, overload, timeout, and temporary-service failures remain retryable, and an upstream `X-Should-Retry` override is honored.
- Tray **Exit** now uses a final shutdown path that always releases the facade listener after attempting configuration recovery, including when recovery must remain pending for the next launch.

## [0.1.16] — 2026-08-16

### Changed

- Reasoning configuration is now fully user- or Grok Build-owned. hellogrok no longer creates, migrates, reorders, or replaces `reasoning_effort`, `reasoning_efforts`, or `supports_reasoning_effort` for any channel; recovery still removes temporary reasoning projections recorded by earlier releases.
- Provider effort values are no longer normalized through a DeepSeek-specific level table. Responses, Chat Completions, and Messages preserve provider-owned values, while Responses-to-Messages bridges omit `none` and `minimal` exactly as Grok Build's native Messages serializer does.

### Fixed

- First-party DeepSeek Messages now converts an explicit off selection to its native thinking switch even when only `reasoning_effort = "none"` is configured. Missing reasoning fields remain untouched and continue to use Grok Build and provider defaults.

## [0.1.15] — 2026-08-16

### Fixed

- Proxy configuration updates now preserve the relative positions of existing `reasoning_effort`, single-line or multiline `reasoning_efforts`, and `supports_backend_search` assignments across every channel. Missing proxy-owned fields use existing assignments as anchors and are inserted in that order; repeated applies remain byte-stable, normal restore remains byte-exact, and legacy reasoning-menu migration preserves existing reasoning/search placement.

## [0.1.14] — 2026-08-16

### Changed

- Every successful proxy apply, restore, or rollback write now saves valid UTF-8 `config.toml` content atomically without a byte-order mark. Read-only inspection still accepts either UTF-8 form without rewriting the file.

### Fixed

- Restore now rejects invalid UTF-8 before line-scoped TOML recovery, reports the configuration path and invalid byte location, and leaves both the configuration and recovery state unchanged for a later retry.

## [0.1.13] — 2026-08-16

### Changed

- `config.toml` may use UTF-8 with or without a byte-order mark. Proxy apply and restore transactions preserve the file's current BOM choice, including a concurrent user encoding edit, while hellogrok-owned state, preference, and cache files remain UTF-8 without BOM.
- Added `hellogrok normalize-config` as an explicit, atomic UTF-8 BOM removal operation. It validates the complete TOML document, refuses invalid UTF-8 or syntax, and does not guess or transcode another encoding.

### Fixed

- A standard UTF-8 BOM no longer prevents proxy startup during configuration-ownership inspection. Genuine TOML failures now include the configuration path and parser line and column; invalid UTF-8 reports its first invalid location without exposing configuration content.

## [0.1.12] — 2026-08-16

### Changed

- Auto-compaction is now budgeted per custom model. hellogrok resolves the model-level, global session, or default 85% preference, reserves the active maximum output plus a 5% context margin with an 8K minimum, and temporarily lowers only unsafe model thresholds. A lower user threshold is never raised.
- Missing capacity is learned without model-name allowlists from actual request output limits, valid upstream model-capacity headers, and unambiguous structured context errors. Trusted learned context windows are temporarily projected onto the model so Grok Build uses the same denominator; privacy-preserving hashed capacity records expire after 30 days.

### Fixed

- Switching between channels with different context and output limits now reloads each model's own compaction budget instead of sharing one global minimum. Active Grok sessions are refreshed after they become idle through bounded, cancellable, coalesced retries, and proxy shutdown cancels those workers before restoring configuration.
- Capacity learning, threshold updates, and context-window projection use the existing recovery transaction. A failed runtime update keeps the previous active configuration and recovery state, while proxy stop restores original bytes, preserves concurrent user edits even when TOML is temporarily invalid, and leaves request-derived output limits in the private cache instead of turning them into a model output cap.

## [0.1.11] — 2026-08-16

### Fixed

- Tray **Exit** now always terminates the application after attempting proxy cleanup, even when configuration restoration reports an ownership, access, or merge conflict. An unresolved recovery transaction remains available for the next launch instead of keeping the tray process alive.
- Proxy-managed settings can now be restored when unrelated user edits make `config.toml` syntactically invalid, including malformed byte-order-mark text and incomplete values. Recovery compares independently parseable managed assignments, preserves user edits byte for byte, and refuses to discard its transaction if a temporary local route cannot be safely removed.

## [0.1.10] — 2026-08-16

### Changed

- First-party DeepSeek Responses and Messages hosted-search requests now stay on their configured native APIs. DeepSeek Chat defaults to a Responses bridge now that the provider officially supports that API, while an explicit `chat_search_dialect` remains authoritative.
- Context capacity is resolved without model-ID allowlists: explicit model/provider configuration wins, otherwise valid upstream `X-Grok-*` metadata or unambiguous structured context-limit fields can supply the window, with Grok Build's catalog and unknown-model fallback left intact.

### Fixed

- DeepSeek reasoning menus now use Grok Build's compact native string-array syntax. Explicit user assignments, including lists without `none`, empty lists, and custom object labels/defaults, remain user-owned and are not replaced or extended by hellogrok. The exact object menu generated by older hellogrok releases is migrated once to the compact form and no longer multiplies across start/stop cycles; `supports_reasoning_effort` is not redundantly projected.
- A provider context-budget rejection caused by Grok Build reserving the full configured completion allowance now lowers only that request's `max_output_tokens` or `max_tokens` to the reported available space and retries once. The retry requires an exact structured, internally consistent `400 invalid_request_error`, so unrelated, malformed, exhausted, or repeated failures still pass through unchanged.
- Valid Responses usage from any channel now gains Grok Build live-context details, including common Chat-compatible token and detail aliases. Incomplete, conflicting, overflowing, or placeholder usage remains null instead of resetting the persisted token baseline; Chat `finish_reason = "length"` is exposed as an incomplete Responses result.
- Restored sessions above the effective threshold now receive trustworthy usage across Responses, Messages, and Chat Completions, allowing Grok Build to compact before the next user prompt instead of failing the provider request with stale or zero context accounting.
- Arbitrary Grok Build local function tools survive all three provider protocol bridges, including shell, file, patch, task, and MCP functions. Third-party request wires consistently remove the xAI-only `x_search` declaration and choice without removing ordinary functions or `web_search`.

## [0.1.9] — 2026-08-15

### Changed

- First-party DeepSeek hosted search now defaults to the official Anthropic Messages API whenever the current request can search and no explicit search dialect is configured. This preserves structured result URLs for Grok Build's native site count, domain summary, and expandable source display; non-search requests retain their configured protocol, and explicit `messages` or `responses` dialects can bridge any backend.
- Responses hosted search now always requests `web_search_call.action.sources` while preserving and deduplicating caller `include` values. Providers that currently ignore the standard hint remain usable, and future support flows into the existing source normalization without a model-specific update.
- Responses-to-Messages history uses Grok Build's stable backend-search summary instead of synthesizing provider server-tool call/result blocks that were never returned on the Messages wire.

### Fixed

- Grok Build WebSearchClient requests no longer force automatic tool choice into a repeated server-search loop, allowing providers to return the final text consumed by the client.
- DeepSeek Responses `action.queries` is exposed through the canonical singular `action.query` field without removing the provider array, so Grok Build can display each search action.
- Messages result metadata now retains `title`, `page_age`, and `encrypted_content`; authoritative provider source lists are no longer mixed with citation-only URLs.
- Search-protocol selection now respects `tool_choice` exclusions, so a request that selects an ordinary function stays on its configured Responses, Messages, or Chat Completions API.

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

### Security

- Release builds now use Go 1.26.6, which fixes the reachable standard-library vulnerabilities `GO-2026-6218`, `GO-2026-6090`, `GO-2026-6089`, `GO-2026-5972`, and `GO-2026-5026` reported against Go 1.26.5 HTTP and TLS call paths.

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

[Unreleased]: https://github.com/hellowind777/hellogrok/compare/v0.1.39...HEAD
[0.1.39]: https://github.com/hellowind777/hellogrok/compare/v0.1.38...v0.1.39
[0.1.38]: https://github.com/hellowind777/hellogrok/compare/v0.1.37...v0.1.38
[0.1.37]: https://github.com/hellowind777/hellogrok/compare/v0.1.36...v0.1.37
[0.1.36]: https://github.com/hellowind777/hellogrok/compare/v0.1.35...v0.1.36
[0.1.35]: https://github.com/hellowind777/hellogrok/compare/v0.1.34...v0.1.35
[0.1.34]: https://github.com/hellowind777/hellogrok/compare/v0.1.33...v0.1.34
[0.1.33]: https://github.com/hellowind777/hellogrok/compare/v0.1.32...v0.1.33
[0.1.32]: https://github.com/hellowind777/hellogrok/compare/v0.1.31...v0.1.32
[0.1.31]: https://github.com/hellowind777/hellogrok/compare/v0.1.30...v0.1.31
[0.1.30]: https://github.com/hellowind777/hellogrok/compare/v0.1.29...v0.1.30
[0.1.29]: https://github.com/hellowind777/hellogrok/compare/v0.1.28...v0.1.29
[0.1.28]: https://github.com/hellowind777/hellogrok/compare/v0.1.27...v0.1.28
[0.1.27]: https://github.com/hellowind777/hellogrok/compare/v0.1.26...v0.1.27
[0.1.26]: https://github.com/hellowind777/hellogrok/compare/v0.1.25...v0.1.26
[0.1.25]: https://github.com/hellowind777/hellogrok/compare/v0.1.24...v0.1.25
[0.1.24]: https://github.com/hellowind777/hellogrok/compare/v0.1.23...v0.1.24
[0.1.23]: https://github.com/hellowind777/hellogrok/compare/v0.1.22...v0.1.23
[0.1.22]: https://github.com/hellowind777/hellogrok/compare/v0.1.21...v0.1.22
[0.1.21]: https://github.com/hellowind777/hellogrok/compare/v0.1.20...v0.1.21
[0.1.20]: https://github.com/hellowind777/hellogrok/compare/v0.1.19...v0.1.20
[0.1.19]: https://github.com/hellowind777/hellogrok/compare/v0.1.18...v0.1.19
[0.1.18]: https://github.com/hellowind777/hellogrok/compare/v0.1.17...v0.1.18
[0.1.17]: https://github.com/hellowind777/hellogrok/compare/v0.1.16...v0.1.17
[0.1.16]: https://github.com/hellowind777/hellogrok/compare/v0.1.15...v0.1.16
[0.1.15]: https://github.com/hellowind777/hellogrok/compare/v0.1.14...v0.1.15
[0.1.14]: https://github.com/hellowind777/hellogrok/compare/v0.1.13...v0.1.14
[0.1.13]: https://github.com/hellowind777/hellogrok/compare/v0.1.12...v0.1.13
[0.1.12]: https://github.com/hellowind777/hellogrok/compare/v0.1.11...v0.1.12
[0.1.11]: https://github.com/hellowind777/hellogrok/compare/v0.1.10...v0.1.11
[0.1.10]: https://github.com/hellowind777/hellogrok/compare/v0.1.9...v0.1.10
[0.1.9]: https://github.com/hellowind777/hellogrok/compare/v0.1.8...v0.1.9
[0.1.8]: https://github.com/hellowind777/hellogrok/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/hellowind777/hellogrok/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/hellowind777/hellogrok/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/hellowind777/hellogrok/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/hellowind777/hellogrok/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/hellowind777/hellogrok/compare/v0.1.1...v0.1.3
[0.1.1]: https://github.com/hellowind777/hellogrok/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/hellowind777/hellogrok/releases/tag/v0.1.0
