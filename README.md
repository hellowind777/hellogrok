<p align="center">
  <img src="./assets/hellogrok.png" alt="hellogrok icon" width="128">
</p>

# hellogrok

A cross-platform local proxy that makes Grok Build custom model channels work with common API formats, native Web tools, isolated authentication, and automatic configuration recovery.

[![Version](https://img.shields.io/badge/version-0.1.12-2f6feb.svg)](./internal/appinfo/appinfo.go)
[![Go](https://img.shields.io/badge/Go-1.26.6-00ADD8.svg)](./go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![Platforms](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey.svg)](#platform-support)
[![LINUX DO](https://img.shields.io/badge/LINUX_DO-recognized-0A84FF?logo=linux&logoColor=white)](https://linux.do)

[English](./README.md) · [简体中文](./README_CN.md) · [Release notes](./RELEASE_NOTES.md) · [Changelog](./CHANGELOG.md)

> 🏅 This project is linked & recognized by the [LINUX DO](https://linux.do) community.

## Contents

- [Why hellogrok](#why-hellogrok)
- [Features](#features)
- [Search and configuration](#search-and-configuration)
- [DeepSeek](#deepseek)
- [Download](#download)
- [Quick start](#quick-start)
- [Platform support](#platform-support)
- [Tray and CLI](#tray-and-cli)
- [Autostart](#autostart)
- [How it works](#how-it-works)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [Limitations](#limitations)
- [Contributing](#contributing)
- [License](#license)

## Why hellogrok

Grok Build can use custom model endpoints, but real-world providers do not all expose the same protocol, response shape, authentication method, or Web-search behavior. A channel that works with `curl` can still fail in a normal Grok Build conversation, lose native Web tools, or receive the wrong login credential.

hellogrok provides one local compatibility layer for those custom channels. It prepares the required Grok configuration while running, keeps each channel tied to its own endpoint and credentials, supports Grok Build's native Web workflows, and restores the original configuration when stopped.

It is intended for users who maintain multiple third-party model channels and want to switch between them from Grok Build without manually rewriting URLs or changing tool configuration for every session.

## Features

[Release notes](./RELEASE_NOTES.md) | [Full Changelog](./CHANGELOG.md)

### Channel compatibility

- Supports upstream channels that use `responses`, `messages`, or `chat_completions`.
- Keeps the provider's configured upstream protocol, URL path, model, credential, reasoning, and tool semantics at the provider boundary.
- When `supports_backend_search = true`, temporarily exposes the channel to Grok Build as Responses, then translates requests, responses, and SSE events to and from the provider's real protocol. Grok Build can therefore consume `web_search_call` across every supported upstream format, plus sources and site counts whenever the upstream returns real result URLs.
- When `supports_backend_search = false`, keeps Grok Build on the configured native consumer so its client `web_search` uses `[models].web_search`, `GROK_WEB_SEARCH_MODEL`, or the authenticated official fallback. Omission preserves Grok Build catalog behavior, except that models on the exact first-party DeepSeek endpoint enable its documented provider-hosted search by default. An explicit false still opts out unless that custom route is selected as the default search model.
- Exposes channel-scoped `/responses`, `/messages`, and `/chat/completions` routes and restores the original `api_backend` byte-for-byte when the proxy stops.
- Validates protocol tool history before forwarding: Responses calls require matching `function_call_output` items, Messages `tool_use` blocks require `tool_result` blocks in the immediately following user message, and Chat tool calls require matching tool messages. Deterministic failures return a non-retryable `400` instead of entering Grok Build's retry loop.
- Converts provider-private `keepalive`, `keep-alive`, `keep_alive`, `heartbeat`, and `ping` frames into standard SSE comments before they reach Grok Build, without consuming Responses sequence numbers, and closes each upstream stream as soon as its protocol terminal event arrives.
- Bounds ordinary upstream response-header waits and gaps between reads from every response body at 601 seconds, one second beyond Grok Build's 600-second shell default. First-party `api.deepseek.com` routes use 660 seconds to cover the provider's documented ten-minute queue. There is no total request deadline, and every upstream byte, including a non-streaming queue newline or normalized heartbeat, renews the idle window.
- Logs the model declared by the raw upstream response before normalization, including terminal-frame precedence, case-insensitive mismatch detection, and conflicting declarations, without changing routing or response data.
- Completes a missing empty `signature` on Messages `thinking` block starts while preserving the provider's later `signature_delta`, so Messages-compatible relays remain consumable by Grok Build's strict native decoder.
- Preserves each configured upstream URL path and model identifier.
- Prepares every explicit custom channel before use, avoiding first-request failures after `/model` switching.
- Preserves portable conversation history during model hot switching while withholding only encrypted reasoning known to belong to a different channel, protocol, wire model, or upstream endpoint.
- Preserves arbitrary Grok Build local function tools, including shell, file, patch, task, and MCP functions, through Responses, Messages, and Chat Completions bridges. Third-party channels never receive xAI-only `x_search`; provider-hosted tools still require real upstream support.

### Native Web tools

- Supports Grok Build's native `web_search` workflow for hosted and client-search modes.
- With `supports_backend_search = true`, every supported format uses the current channel's own hosted search: Responses stays Responses, Messages stays Messages with `web_search_20250305`, and Chat uses its configured search dialect or protocol bridge. First-party DeepSeek follows the configured native protocol now that its Responses API is GA; Chat hosted search defaults to a Responses bridge because DeepSeek Chat documents function tools only. Explicit `chat_search_dialect = "messages"` remains available when a Messages bridge is required.
- For a channel marked `supports_backend_search = true`, the Grok Build side receives canonical Responses search events when the upstream actually executes search, including completed `web_search_call` items and any verified sources, citations, and usage the provider returned. The flag is a routing declaration, not a capability probe. Every Responses search asks for complete `web_search_call.action.sources` metadata while preserving caller entries. DeepSeek currently accepts but silently ignores that standard `include` hint, so hellogrok consumes any URLs already present in its native search action but cannot recover omitted results. If DeepSeek implements the hint later, the same request begins receiving sources without a hellogrok update. Messages search-result/citation blocks, provider-level citations, and Chat annotations/search-result containers are normalized into the same canonical source fields.
- Selecting a proxied custom route through `[models].web_search` or `GROK_WEB_SEARCH_MODEL` projects that route as `supports_backend_search = true` for the entire proxy lifetime, even when its original value is false. Grok Build can therefore discover hosted `web_search` in ordinary sessions and its fixed non-streaming WebSearchClient request; hellogrok translates both to the provider protocol and restores the original setting on stop.
- Preserves real search URLs from adapted search results in both `web_search_call.action.sources` and `output_text.annotations`, including valid final-answer links only when the response independently confirms that search ran. This lets Grok Build display its native deduplicated site count.
- Accepts an adapted client-search response only when the upstream independently proves that search completed and also returns non-empty answer text. Providers that silently ignore their search extension receive a non-retryable `502`; hellogrok cannot manufacture search capability for such a channel.
- Keeps `web_fetch` available as an independent tool when allowed by the active agent configuration.
- Applies the same search behavior to supported subagents.
- Keeps official Grok models on Grok Build's native search and login path.

### Authentication and configuration safety

- Uses channel-owned API keys, environment keys, authentication providers, and headers.
- Prevents an official Grok login token from being sent to an unrelated custom channel.
- Validates channel-owned header names and values while loading configuration. Request framing, content, and connection headers remain controlled by the proxy.
- Checks and temporarily completes required Grok settings when the proxy starts.
- Computes an independent auto-compaction budget for each custom model from its effective context window and maximum output. It temporarily lowers only unsafe thresholds, never raises a lower user value, and restores every managed value when the proxy stops.
- On normal stop, tray exit, Ctrl+C, SIGTERM, or failed startup, restores untouched temporary values while preserving concurrent user edits through a field-level three-way merge. If unrelated edits make the full TOML document invalid, line-scoped recovery still restores independently valid managed assignments without rewriting the invalid user bytes.
- Always honors the tray **Exit** command after attempting cleanup. If safe restoration is impossible because the file cannot be accessed or an unowned local route remains, the recovery transaction stays on disk for the next launch instead of trapping the user in the tray process.
- Recovers proxy-managed settings after an unclean exit with `hellogrok restore`.

### Desktop and operations

- Provides a native Windows tray application and a console CLI.
- Remembers the user's proxy-enabled choice between tray launches.
- Defaults to proxy-enabled on first launch so new users see a working proxy immediately.
- Includes login autostart controls for Windows, Linux, and macOS.
- Provides route inspection, grouped status, live log search, usage-day log retention, and terminal log following.
- Builds for Windows, Linux, and macOS on amd64 and arm64.

hellogrok is a Grok Build channel proxy. It is not a system proxy, PAC service, VPN, or general HTTPS interceptor.

## Search and configuration

### Search modes

Search behavior follows the explicit search-model selection first, then the active backend:

| Setting | Search behavior |
|---------|-----------------|
| `[models].web_search` or `GROK_WEB_SEARCH_MODEL` is set | If the selected model is a proxied custom route, hellogrok temporarily projects it as `supports_backend_search = true` and Responses-facing for both ordinary sessions and Grok Build's WebSearchClient. The route may use `responses`, `messages`, or `chat_completions` upstream. The environment variable takes precedence, selection overrides an original false only while the proxy runs, and no startup request is sent. |
| Any channel with `supports_backend_search = true` | Grok Build uses Responses hosted tools while hellogrok calls that channel's own working search API: Responses, Messages `web_search_20250305`, or the selected Chat search dialect/bridge. |
| Any channel with `supports_backend_search = false` | Grok Build uses client `web_search`: first `[models].web_search` or `GROK_WEB_SEARCH_MODEL`, otherwise its authenticated official fallback. A selected custom search route is temporarily overridden to true; models on the exact first-party DeepSeek endpoint also default to native hosted search when the field is omitted. |
| No usable hosted or client search path | `web_search` is unavailable for that model. |
| `web_fetch` | Remains independent of the search-model selection and follows the active tool permissions. |

Backend search is not a second Grok Build request. Its declaration is part of the active model request, so hellogrok must select the provider protocol before the model decides whether to search. Client `web_search` is different: after the active model calls the local function, Grok Build's `WebSearchClient` makes a separate non-streaming `/responses` request to the selected search model, which hellogrok then maps to that route's search protocol.

hellogrok never creates, selects, or replaces `[models].web_search`, and startup sends no search-capability probes. A Messages channel marked enabled must actually support `web_search_20250305`. Chat defaults to `web_search_options`; official xAI and first-party DeepSeek Chat bridge to Responses. Set `chat_search_dialect` to `web_search_options`, `search_parameters`, `messages`, or `responses` when a route needs an explicit strategy. The temporary flag is a routing declaration, not proof of provider capability. A real upstream HTTP rejection is preserved; if the fixed WebSearchClient request is silently answered without a completed search, hellogrok returns a non-retryable `502` stating that the selected search model did not complete backend `web_search`.

For client search, hellogrok clarifies tool descriptions but never infers or forces tool use from prompt text. Mandatory selection comes only from structured `tool_choice`. Internal wire aliases are changed only in protocol-defined tool declarations, choices, and call-name fields; response text, URLs, tool arguments, tool results, and other business JSON remain unchanged.

### Example configuration

This example selects a Chat Completions channel that supports `web_search_options` as Grok Build's default search model:

```toml
[models]
web_search = "search-relay"

[model.search-relay]
model = "provider-search-model"
base_url = "https://api.example.com/v1"
env_key = ["SEARCH_RELAY_API_KEY"]
api_backend = "chat_completions"
chat_search_dialect = "web_search_options"
supports_backend_search = false
```

The original false in this example remains user-owned, but selection temporarily overrides it to true while hellogrok runs and restores it on stop. The provider must still support the selected search dialect. For a channel not selected as the default search model, set `supports_backend_search = true` only after confirming its provider API supports hosted search. Messages uses `web_search_20250305`; Chat relays can select an explicit `chat_search_dialect`.

### Supported channel settings

| Setting | Required | Default | Purpose |
|---------|----------|---------|---------|
| `model` | No | Model table ID | Model identifier sent to the upstream channel. |
| `base_url` or `api_base_url` | Yes | None | Custom upstream endpoint. Models without a custom URL are not proxied. |
| `api_backend` | No | Model catalog, then `chat_completions` | Native upstream API format: `responses`, `messages`, or `chat_completions`. A capable non-Responses channel is temporarily projected as Responses only to Grok Build; hellogrok converts at the provider boundary while preserving the selected native protocol unless an explicit search bridge applies. |
| `chat_search_dialect` | No | Host-based | Hosted-search strategy override: `web_search_options`, `search_parameters`, `messages`, or `responses`. Official DeepSeek and xAI Chat default to `responses`; other Chat routes default to `web_search_options`. Responses and Messages stay native unless this field explicitly requests a bridge. |
| `api_key` | One auth method | None | Static channel credential. Prefer `env_key` for shared configurations. |
| `env_key` | One auth method | None | Environment variable name or ordered list of names containing the channel credential. |
| `auth_provider` | One auth method | None | Grok command-based authentication provider. |
| `auth_scheme` | No | `bearer` | Upstream authentication scheme. Set `x_api_key` only for providers that explicitly require `X-Api-Key`. |
| `extra_headers` | No | Empty | Additional channel-owned HTTP headers, including provider-specific authentication. Proxy-controlled framing, content, and connection headers are rejected; names are case-insensitive. |
| `env_http_headers` | No | Empty | HTTP headers populated from environment variables. Resolved values use the same header rules as `extra_headers`. |
| `supports_backend_search` | No | Catalog/provider default | When true, uses this channel's own hosted search and exposes canonical Responses search events to Grok Build for all three upstream formats. When false, Grok Build uses its configured or authenticated client-search route unless this custom model is selected by `[models].web_search` or `GROK_WEB_SEARCH_MODEL`, which temporarily overrides it to true. The exact first-party DeepSeek endpoint defaults to true independently of model ID. Other unknown models remain owned by explicit configuration or Grok Build's model catalog. |
| `context_window` | No | Provider/model catalog | Total input-plus-output context capacity. An explicit model/provider value wins; otherwise hellogrok can learn a trustworthy upstream value and temporarily project it at model level so Grok Build uses the same auto-compaction denominator. |
| `max_completion_tokens` | No | Model metadata | Maximum generated-token allowance. An explicit value wins. Without one, hellogrok observes the actual outgoing request and trusted upstream metadata for budgeting, but a request-derived value is not written back as a model output cap. |
| `auto_compact_threshold_percent` | No | Model value, then `[session]`, then `85` | Preferred percentage of `context_window` at which Grok Build compacts. hellogrok keeps it when safe and temporarily lowers it per model when the output reserve and safety margin require an earlier trigger. Valid range: `0` to `100`. |
| `inference_idle_timeout_secs` | No | Grok Build/provider policy | Maximum idle gap while waiting for upstream headers or body data. A model value overrides the global `[models]` value; first-party DeepSeek uses 660 seconds when no explicit value exists. |

Model settings may be declared directly under `[model.<id>]` or inherited from a referenced `[model_providers.<id>]`. Model-level values take precedence. When `api_backend` is absent from both, hellogrok leaves it absent in Grok Build's active configuration and follows the concrete protocol Grok Build resolves from its current model catalog; a model unknown to that catalog falls back to Grok Build's `chat_completions` default. A hosted-search tool present on that resolved Responses request is treated as the capability signal, so a future catalog entry does not require a model-ID update in hellogrok.

Quote the full ID when a channel ID contains a dot, for example `[model."provider.v1-beta"]`. Dashes do not require quoting. A value such as `name = "Provider.v1-beta"` is display-only and may use dots or dashes directly. While active, hellogrok also accepts a legacy unquoted dotted header and restores the original text on stop.

Do not manually set a custom channel URL to hellogrok's local address. The application manages temporary local URLs only while the proxy is active.

### Automatic compaction capacity

Grok Build calculates its trigger from the full context window: `context_window * auto_compact_threshold_percent / 100`. It does not subtract the completion allowance first. hellogrok therefore calculates a model-specific safe percentage before Grok samples the next turn:

```text
margin    = max(8192, ceil(context_window / 20))
safe      = floor(100 * (context_window - max_completion_tokens - margin) / context_window)
effective = min(user_or_default_threshold, safe)
```

The preferred threshold comes from `[model.<id>]`, then `[session]`, then Grok Build's default `85`. The 5% margin, with an 8K minimum, covers token-estimation and compaction-summary overhead. If both capacities are known and the preferred value is already lower, hellogrok leaves it unchanged. If the output reserve plus margin consumes the whole window, no safe positive percentage exists; hellogrok reports a capacity conflict and keeps the existing threshold instead of injecting `0`.

Explicit model/provider capacity remains authoritative. Missing values are learned from the actual outgoing `max_output_tokens` or `max_tokens`, valid upstream `X-Grok-Context-Window` and `X-Grok-Max-Completion-Tokens` headers, or one unambiguous structured context limit. A trustworthy learned window is temporarily written as model-level `context_window`, because the threshold is only correct when Grok Build uses the same denominator. Request-derived output limits participate in the calculation but are not written as `max_completion_tokens`.

When one or both capacities are still unknown, hellogrok marks the model as learning and does not guess a threshold from its name. The first request may therefore still use Grok Build's catalog or fallback values; the existing structured context-error path can reduce that request's output allowance and retry once. Learned records use hashed route identities, contain no URL, model name, or credential, expire after 30 days, and are stored as `capacity_cache.json` in the private hellogrok data directory.

Every threshold and learned-window projection uses the same recovery transaction as the local route. Model-catalog reload and current-model reselection make an idle open session adopt the update. Active sessions are retried after bounded delays until they become idle; stopping the proxy cancels the worker before restoring the original configuration. `GROK_AUTO_COMPACT_THRESHOLD_PERCENT` still has higher priority than TOML inside Grok Build. When hellogrok inherits that environment variable it can warn about an unsafe value, but it cannot replace the environment of an already running Grok process.

## DeepSeek

DeepSeek compatibility is enabled by the exact first-party host `api.deepseek.com`, independently of model ID. Rolling aliases and future models therefore inherit authentication, endpoints, hosted search, protocol-level request normalization, queue keep-alives, usage handling, and SSE framing without a hellogrok release. Native same-protocol requests retain unknown fields; cross-protocol bridges map the fields Grok Build currently sends that the target protocol can express. Unknown future reasoning-effort values remain intact. As documented on 2026-08-16, the current production IDs are the GA `deepseek-v4-pro` and `deepseek-v4-flash`; these names are examples, not a hellogrok allowlist. A relay that merely reuses a DeepSeek model name receives no first-party assumptions.

### Recommended configuration

Responses is the closest native match for Grok Build and is the recommended backend:

```toml
[model.deepseek-v4-pro]
model = "deepseek-v4-pro"
base_url = "https://api.deepseek.com"
env_key = ["DEEPSEEK_API_KEY"]
api_backend = "responses"
context_window = 1048576
max_completion_tokens = 384000
inference_idle_timeout_secs = 660

[model.deepseek-v4-flash]
model = "deepseek-v4-flash"
base_url = "https://api.deepseek.com"
env_key = ["DEEPSEEK_API_KEY"]
api_backend = "responses"
context_window = 1048576
max_completion_tokens = 384000
inference_idle_timeout_secs = 660
```

Leave `supports_backend_search` unset to use DeepSeek's native hosted Web Search, or set it explicitly to `false` to use Grok Build client search instead. Responses and Messages hosted-search requests stay on their configured native APIs. Chat defaults to a Responses bridge because DeepSeek Chat currently documents only function tools; set `chat_search_dialect = "messages"` only when a Messages bridge is intentionally required. hellogrok asks Responses for `web_search_call.action.sources`, retains any URL DeepSeek actually returns, and never invents omitted source details. Native Responses and Chat authenticate with Bearer. Messages uses the documented `https://api.deepseek.com/anthropic` SDK base URL (the effective endpoint is `/anthropic/v1/messages`) and `X-Api-Key`; the `[1m]` suffix is accepted only as DeepSeek's documented Anthropic integration alias and must not be used with Responses.

The two capacity limits above reflect DeepSeek's documented 1M total context and 384K maximum output; `1,048,576` is the service boundary observed in its structured context error, not a model-ID constant in hellogrok. With these values, an 85% preference is temporarily limited to 58% by the automatic budget. The 660-second value is an idle policy chosen to cover DeepSeek's documented ten-minute queue. A model-level value takes precedence over an inherited provider value. With no explicit limit, hellogrok accepts valid upstream `X-Grok-Context-Window` and `X-Grok-Max-Completion-Tokens` metadata, or an unambiguous structured context-limit field from an error, without guessing from model names. Grok Build's own catalog remains the final fallback; an unknown custom model with no `context_window` defaults to 200,000 tokens until hellogrok learns a trustworthy window. Because DeepSeek does not publish the Grok-private response headers, explicitly configure both values for predictable first-turn behavior. Future models and rolling aliases can change these values without a hellogrok release.

The current DeepSeek reasoning menu has `High` selected by default. Grok Build natively accepts the compact `reasoning_efforts = ["none", "low", "high", "max"]` form and derives reasoning support from a non-empty menu, so hellogrok does not add the redundant `supports_reasoning_effort` flag or four verbose object tables. hellogrok treats every explicit user assignment as user-owned, including a list without `none`, an empty list, or custom object labels/order/defaults, and does not replace or extend it. Current Grok Build ignores an empty collection override while merging catalog defaults, so `reasoning_efforts = []` guarantees that hellogrok injects nothing but does not necessarily erase a menu inherited by Grok Build itself. The exact verbose menu written by older hellogrok releases is migrated once to the compact form; an existing model/global effort selection is preserved, otherwise `reasoning_effort = "high"` keeps the old default. The official wire mapping remains consistent across all three protocols: `minimal`/`low` map to `low`, `medium`/`high`/`xhigh` map to `high`, `max` stays `max`, and `none` disables thinking. (`minimal` is a Responses-only input value.)

### Native capability mapping

| DeepSeek surface | hellogrok behavior in Grok Build |
|------------------|----------------------------------|
| Responses | Preserves `instructions`, developer messages, reasoning, native `text.format` JSON Schema, function tools, and native Web Search. It sends the standard source `include` hint without inventing missing results; source display is limited to URLs DeepSeek actually returns. Provider `action.queries` arrays remain intact while each call gains the singular `action.query` field Grok Build displays. It handles `response.completed`, `response.incomplete`, and `response.failed` as terminal events. |
| Chat Completions | Preserves tool-call `reasoning_content`, requests the terminal streaming usage block, maps `max_completion_tokens` to `max_tokens` unless the latter is already explicit, converts developer messages to system messages, supplies non-null assistant tool content, and maps an explicit Responses `user` to DeepSeek `user_id`. DeepSeek documents function tools only on this surface, so hosted Web Search requires a Messages or Responses bridge. DeepSeek rejects `tool_choice` while thinking is enabled, so the selector is removed while function declarations remain available for automatic use; an explicit non-thinking request keeps the documented selector forms. Grok Build `json_schema` becomes documented `json_object` mode plus a schema instruction, and Grok Build validates the returned JSON locally. |
| Anthropic Messages | Uses `X-Api-Key`, preserves thinking/tool history, effort control, function tools, and native server Web Search blocks. An omitted Grok Build `None` selection becomes `thinking.type=disabled` instead of falling through to DeepSeek's default `high`; only the supported `output_config.effort` is sent, explicit Responses `user` maps to `metadata.user_id`, and the documented `deepseek-v4-pro[1m]` alias remains on the Messages wire. |
| Queue and usage | Accepts non-streaming empty-line keep-alives and streaming `: keep-alive` comments. Real terminal usage is preserved so Grok Build can drive context accounting and auto-compaction. |

Responses is the only DeepSeek surface here with native JSON Schema output. Chat supports only `json_object`, can occasionally return empty content, and relies on the injected schema instruction plus Grok Build's local validator. Chat function `strict: true` is a separate Beta feature: configure `base_url = "https://api.deepseek.com/beta"` when that exact Chat behavior is required. Messages structured output continues through Grok Build's validated `StructuredOutput` function because DeepSeek's Anthropic-compatible `output_config` supports effort only. Explicit caller-supplied user isolation IDs are preserved across protocol bridges, but hellogrok does not invent one or derive one from unrelated identity headers. A Chat `insufficient_system_resource` finish is surfaced as a structured failure, or as retryable HTTP `503` when the native non-streaming protocol can still return an HTTP error.

The provider's API surface is broader than the part Grok Build can address. DeepSeek's current Responses compatibility table marks `include`, image/file input, previous responses, server-side conversations, background mode, truncation, and `stream_options` unsupported; native `file_search`, `code_interpreter`, `computer_use`, and `mcp` tool types are ignored. Unsupported fields are currently accepted and silently ignored, so hellogrok retains forward-compatible standard fields such as the Web Search source `include` hint. This does not disable Grok Build's local shell, MCP, task, file, or `apply_patch` tools: current Grok Build source sends them as supported function tools. DeepSeek also documents a custom tool shape named `apply_patch`; hellogrok leaves that shape intact when a caller supplies it, but current Grok Build source does not dispatch the resulting `custom_tool_call` to its local patch executor, so this passthrough is not presented as a local editing path. DeepSeek's Anthropic API does not support image/document input, code execution blocks, or native MCP blocks. Prefix continuation and FIM exist in DeepSeek's API, but hellogrok does not invent Grok Build UI or completion endpoints that the agent does not call.

Caching stays provider-native. hellogrok preserves ordered conversation/tool/reasoning history and Grok Build's `x-grok-conv-id` on all three protocols; it keeps Responses cache keys stable, preserves native Messages `cache_control` markers, and adds the same system/current-tip/previous-turn markers when converting Responses to Messages. Historical backend searches use Grok Build's stable text summary instead of synthetic Messages server-tool blocks, while live calls and results remain provider-native. DeepSeek ignores Responses cache-key parameters and manages its prefix cache automatically, so those inert fields are left stable rather than treated as a cache switch. Cache usage fields are preserved or projected into the receiving protocol, but only a real provider response can prove a cache hit.

## Download

Download the latest tagged build from [GitHub Releases](https://github.com/hellowind777/hellogrok/releases/latest). Windows releases provide separate tray and console executables; Linux and macOS releases provide the standard foreground CLI.

| Platform | Release assets |
|----------|----------------|
| Windows amd64 / arm64 | `hellogrok-windows-<arch>.exe` and `hellogrok-cli-windows-<arch>.exe` |
| Linux amd64 / arm64 | `hellogrok-linux-<arch>` |
| macOS Intel / Apple Silicon | `hellogrok-darwin-<arch>` |

Every binary has a neighboring `.sha256` file. On Windows, verify an amd64 tray build before running it:

```powershell
$artifact = ".\hellogrok-windows-amd64.exe"
$expected = ((Get-Content -LiteralPath "${artifact}.sha256") -split '\s+')[0]
$actual = (Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash.ToLowerInvariant()
$actual -eq $expected
```

The final command must print `True`. On Linux use `sha256sum -c <file>.sha256`; on macOS use `shasum -a 256 -c <file>.sha256`. Release binaries are currently unsigned, so checksum verification establishes file integrity but not publisher identity.

## Quick start

### Prerequisites

- Grok Build with a readable `~/.grok/config.toml` containing at least one custom model URL.
- A valid credential source for every custom channel.
- Go **1.26.6** when building from source.

Compatibility is verified against the latest fetched Grok Build `origin/main`, not against an older release branch. The exact tested revision is reported with the test results because the local Grok Build checkout may update independently.

Set `GROK_HOME` to use a Grok configuration directory other than `~/.grok`.

### Windows

```powershell
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
.\scripts\build.ps1
.\dist\hellogrok-cli.exe routes
.\dist\hellogrok.exe
```

Use the tray menu to select **Start proxy**. New Grok Build processes read the proxy configuration directly; idle custom-model sessions already attached to a shared leader are hot-switched automatically.

### Linux or macOS

```bash
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -o dist/hellogrok ./cmd/hellogrok
./dist/hellogrok routes
./dist/hellogrok start
```

Expected startup output includes a local channel endpoint and a successful configuration rewrite. Keep the process running while using Grok Build. Ctrl+C or SIGTERM stops the proxy and restores the original configuration.

### First-use checklist

1. Run `hellogrok routes` and confirm every intended custom model is listed with the correct backend and an available authentication source.
2. Start hellogrok. If Grok Build is already open, inspect the shared-leader hot-switch result in status or logs.
3. Start with a unique, non-sensitive detail, switch through the intended models with `/model`, and confirm a later model can refer to the visible conversation history.
4. Test `web_search` and `web_fetch` separately according to the selected search mode.
5. Stop hellogrok normally and confirm Grok Build's configuration no longer points to the local proxy.

## Platform support

| Platform | Standard interface | Tagged release artifacts | Architectures |
|----------|--------------------|--------------------------|---------------|
| Windows | Native tray and CLI | GUI and console `.exe` files | amd64, arm64 |
| Linux | Foreground CLI or systemd user service | CLI binary | amd64, arm64 |
| macOS | Foreground CLI or LaunchAgent | CLI binary | amd64, arm64 |

Standard release binaries use `CGO_ENABLED=0`. Tagged releases are configured to include SHA-256 checksum files.

Linux and macOS users can build the optional tray interface from source:

```bash
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

Linux tray builds require GTK 3 and AppIndicator development packages. macOS tray builds require Xcode Command Line Tools. The standard Unix CLI does not require these desktop dependencies.

Current Windows and macOS artifacts are not code-signed or notarized.

## Tray and CLI

### Tray controls

The Windows tray application and optional Unix tray build provide:

- **Start proxy** — enabled by default on first launch; later starts and stops remember the selected state.
- **Autostart** — enables or disables login startup.
- **Status and logs** — opens the current status and live log window.
- **Exit** — attempts to restore the configuration and stop the proxy, then always exits the tray process. An unresolved recovery transaction remains available for the next launch.

Only one tray instance runs in a login session; launching it again exits immediately instead of creating a second tray. The remembered tray state is independent from the foreground `hellogrok start` command.

On Windows, the divider in **Status and logs** contains a retention selector and log search. Retention counts distinct dates on which hellogrok actually wrote logs rather than elapsed calendar days; the default keeps the latest 7 usage days, with `off`, 3, 7, 14, and 30 available. Cleanup runs at the next application start. Repeated **Search** clicks move to the next match and wrap to the beginning. Status text wraps; raw log lines remain unwrapped for reliable scanning.

**Stop protection**: The **Start proxy** toggle and foreground signal handler remain fail-closed when another provider manager owns Grok Build or a temporary hellogrok route cannot be restored safely. The tray **Exit** command is different: it always terminates after the cleanup attempt, so an external ownership conflict can never trap the user in the application.

Configuration edits made while the proxy is active are merged field by field during shutdown. Values still matching hellogrok's temporary projection are restored to their startup values, while user-edited values and deleted model channels are preserved. When unrelated edits leave TOML temporarily invalid, hellogrok compares each managed assignment independently, restores only values it still owns, preserves the malformed user text byte for byte, and checks textually for remaining local routes. A cleanup that still cannot prove the route safe leaves the recovery state intact; tray exit nevertheless completes.

### Compatibility with CC Switch

CC Switch and hellogrok can run at the same time only when CC Switch is not managing Grok Build. CC Switch's Grok Build proxy takeover and provider switch both write `~/.grok/config.toml`; using either operation while hellogrok owns that file creates a configuration-ownership conflict even though the proxies listen on different ports.

- hellogrok refuses to start when it detects CC Switch's Grok Build takeover marker (`PROXY_MANAGED` on its `/grokbuild/v1` route).
- If CC Switch takeover is enabled after hellogrok starts, a normal proxy stop remains deferred until CC Switch releases Grok Build. Tray **Exit** still closes hellogrok and retains its recovery transaction; releasing CC Switch first remains the cleanest shutdown order.
- If a provider manager completely replaces the live Grok config and no hellogrok route remains, hellogrok preserves the external config and relinquishes its obsolete recovery state.
- CC Switch may continue managing Claude, Codex, Gemini, and other applications while hellogrok is active.

If both Grok proxies were enabled accidentally, disable CC Switch's Grok Build takeover first, then stop hellogrok. Avoid switching the CC Switch Grok Build provider while hellogrok is active.

### CLI reference

| Command | Purpose |
|---------|---------|
| `hellogrok start` | Run the proxy in the foreground. |
| `hellogrok version` | Print the installed version. |
| `hellogrok routes` | List custom routes without printing credentials. |
| `hellogrok restore` | Restore proxy-managed settings after an unclean exit. |
| `hellogrok autostart enable` | Enable login autostart for the current executable. |
| `hellogrok autostart disable` | Disable login autostart. |
| `hellogrok autostart status` | Show the current autostart state. |
| `hellogrok log` | Print and open the log file. |
| `hellogrok logview` | Follow the log in the current terminal. |
| `hellogrok help` | Show command help. |

### Runtime data

| Platform | Location |
|----------|----------|
| Windows | `%LOCALAPPDATA%\hellogrok` |
| Linux and macOS | `~/.hellogrok` |

Runtime data contains application preferences, logs, the recovery state used to restore managed configuration, and `reasoning_provenance.json`. The provenance index stores only SHA-256 digests of opaque reasoning values and route signature domains; it never stores raw reasoning, channel IDs, model names, upstream URLs, or credentials.

Log retention is applied on every platform. The native retention selector and in-window search are currently Windows-only because the standard Linux and macOS builds use terminal log viewing instead of the Win32 status window.

## Autostart

### Windows

Enable **Autostart** from the tray or run `hellogrok autostart enable`. Login startup opens the tray and applies the remembered proxy-enabled state.

### Linux

The standard CLI registers a systemd user service. Enable it and start it immediately with:

```bash
./dist/hellogrok autostart enable
systemctl --user start hellogrok.service
systemctl --user status hellogrok.service
```

### macOS

The standard CLI registers a per-user LaunchAgent. Enable it and load it immediately with:

```bash
./dist/hellogrok autostart enable
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.hellogrok.proxy.plist"
```

Autostart records the current executable's absolute path. Disable and re-enable it after moving the binary. Credentials referenced by `env_key`, `env_http_headers`, or `GROK_HOME` must be available in the login-started process environment, not only in the current shell.

## How it works

```text
Grok Build
    |
    v
hellogrok local channel proxy
    |
    v
Configured custom API channel
```

At startup, hellogrok validates each custom channel and points its URL to a channel-scoped local route. Channels without backend search keep their configured Grok Build consumer and native stream format unless selected as the default search model. Capability-enabled and selected-search channels are temporarily projected as Responses because that is the Grok Build consumer that serializes hosted tools and renders structured search results; hellogrok then translates at the provider boundary and restores the original configuration on stop.

Responses providers remain Responses. Messages providers receive Messages requests and return Responses events through a bidirectional converter. Chat providers use `web_search_options` or `search_parameters`, or bridge to Messages/Responses when configured. Grok Build's fixed non-streaming `WebSearchClient` request uses the same provider adapters when a custom channel is selected as the client-search model.

Native `web_search`, `web_fetch`, official Grok login behavior, and supported subagent workflows remain controlled through Grok Build rather than being replaced by a separate search service.

## Troubleshooting

### No custom routes are found

Confirm that the intended `[model.<id>]` or referenced provider has a valid `base_url` or `api_base_url`. Official models without a custom URL are intentionally excluded.

### `web_search` is unavailable

Check the startup log for the channel's Build and upstream protocols, then inspect the first failing search request. A Responses channel marked enabled must implement the Responses hosted tool; Messages must support `web_search_20250305`; Chat must support its selected `chat_search_dialect`. A channel marked false instead needs a valid `[models].web_search` / `GROK_WEB_SEARCH_MODEL` selection or usable official xAI credentials. Selecting that custom route temporarily makes it backend-search enabled, so a provider rejection or the non-retryable “did not complete backend web_search” error means the upstream capability is missing; the proxy will not hide it with another fallback. `web_fetch` is independent but can still be removed by active tool permissions.

### A request returns 401, 403, or 502

Run `hellogrok routes` and inspect **Status and logs**. Confirm the channel URL, backend, credential source, model identifier, and provider availability. An upstream outage, rate limit, unsupported payload, or stripped search tool must be fixed by the provider or relay.

A 502 can also mean that an upstream returned a malformed success response. hellogrok validates the minimum Responses, Messages, or Chat Completions envelope before forwarding it; the log identifies the missing or invalid field.

Prefer `[model."full.ID"]` when a channel ID contains dots. TOML interprets an unquoted `[model.foo.bar]` as nested tables, so Grok Build originally sees only `foo`; hellogrok temporarily normalizes and validates that header while enabled. Dots or dashes in `name` do not participate in authentication.

### `tool_use` IDs have no immediately following `tool_result`

This provider error means the Messages conversation history is structurally invalid: every assistant message containing one or more `tool_use` blocks must be followed immediately by one user message whose leading `tool_result` blocks resolve that entire batch. hellogrok validates native history and also groups parallel Responses calls/results into one adjacent Messages assistant/user pair before the provider call. Missing results still return a non-retryable `400`; they are never invented because that would corrupt tool state.

### Messages reports `serialization error: missing field signature`

Restart the proxy with the current build. Some Messages-compatible relays omit the required empty `signature` from a streamed `thinking` block start and provide the real opaque value later through `signature_delta`. Grok Build's native Messages decoder rejects that incomplete start before it can consume the delta. hellogrok completes only the missing protocol field and preserves the real delta unchanged for subsequent turns. If the relay never sends a real signature at all, verified hidden reasoning cannot be reconstructed downstream and the provider must fix its Messages response.

### Output arrives all at once

For a streaming request, hellogrok sends `stream=true` to the selected provider API. Non-capable channels keep native SSE. Capable Messages and Chat channels are translated incrementally into Responses events so Grok Build can consume reasoning, text, function calls, `web_search_call`, sources, and terminal status. If the log reports a buffered fallback, the upstream returned one complete JSON response and true streaming was unavailable for that request.

Current Grok Build has two Responses source paths: hosted search reads `web_search_call.action.sources`, while its client `web_search` tool reads URL citations from `output_text.annotations`. For hosted search, non-empty source URLs produce the collapsed `(N sites)` suffix and a numbered expandable body; with no URLs the item has no source count or expandable content. Before forwarding Responses hosted search, hellogrok adds `web_search_call.action.sources` to `include` without replacing caller entries. DeepSeek currently accepts but silently ignores this hint, so its native Responses search action is normalized directly and source display is limited to URLs it actually returns. Keeping the hint makes later DeepSeek support automatic. Responses-level `citations`, Messages search-result/citation blocks, and Chat `annotations`, `citations`, `search_results`, or `web_search_results` are normalized into both Grok Build paths. The WebSearchClient adapter uses the same normalization and, only when search execution is independently confirmed, recovers valid HTTP(S) links from the final answer. A normal answer link does not create a search call. If a provider returns no real URL, search activity can still be shown but a trustworthy site count cannot be fabricated.

### `unknown variant keepalive` or an endless `Waiting for response...`

Upgrade both hellogrok executables to the same current release or build, then restart the proxy. Some relays inject private `keepalive`, `keep-alive`, `keep_alive`, `heartbeat`, or `ping` events into an SSE stream. Grok Build's strict Responses deserializer rejects such JSON events even while upstream generation continues. hellogrok absorbs these names from the SSE `event:` field, JSON `type` or `event`, raw data payloads, and empty-data heartbeat frames, then emits the standards-compatible `: keepalive` comment. A completed Responses event, Messages `message_stop`, or Chat Completions `[DONE]` also closes the upstream request immediately rather than waiting for the provider socket.

The completion log includes `heartbeats=<count>`. If the same error remains while that counter stays zero, confirm that Grok Build is routed through the current proxy with `hellogrok routes`; the provider is likely emitting a different private event name that should be diagnosed from a credential-free stream capture rather than added as a model-specific workaround.

For ordinary channels, hellogrok waits at most 601 seconds for upstream response headers and at most 601 seconds between reads from any response body, including non-streaming JSON and error bodies. This stays one second beyond Grok Build's 600-second shell default, so Grok Build remains the owner of its native timeout classification. First-party `api.deepseek.com` routes use 660 seconds because DeepSeek can queue a request for up to ten minutes. A non-streaming queue newline or streaming heartbeat counts as data and renews that idle window. A timeout before response headers or while reading a non-streaming body returns retryable `504`; after a `200` stream has begun, hellogrok emits the receiving protocol's `proxy_stream_error` and closes the upstream. There is no total request timeout, so an active long-running response is not terminated. The `response_model` log line shows the upstream-declared and configured models; `mismatch=true` identifies a relay that silently substituted a model, while `conflict=true` means different response frames declared different models.

### Automatic compaction never starts

Grok Build does not add each response's `usage.total_tokens` to a running sum. On a terminal Responses event, the current public source first replaces the typed `total_tokens` with `usage.context_details.input_tokens + usage.context_details.output_tokens` when both private fields are valid; without that extension, the provider's `total_tokens` passes through. Chat state then treats the resulting value as the current complete-context measurement, replaces its token baseline with it, adds only locally estimated items produced since that response, and checks the configured auto-compaction threshold before the next model sample.

hellogrok applies that contract uniformly instead of by provider name. A complete, valid Responses pair may use `input_tokens` / `output_tokens` or the common `prompt_tokens` / `completion_tokens` aliases; detail containers are normalized the same way. Messages and Chat usage converted to Responses follows the same rule. Only a complete trustworthy pair receives `context_details`; missing, conflicting, negative, fractional, overflowing, or placeholder measurements remain `usage: null` so Grok Build keeps its previous baseline.

DeepSeek's 1M context is a shared input-plus-generated-output budget, and Responses `max_output_tokens` includes both hidden reasoning and visible output. Reaching that per-response cap produces `status = "incomplete"` with reason `max_output_tokens`; Chat reports `finish_reason = "length"`, which hellogrok maps to the same incomplete result. The partial response remains usable, but it may contain little visible text if reasoning consumed the allowance. Continue in another turn, request a smaller reasoning effort, or raise the output allowance only when the remaining total context permits it. Output-cap exhaustion alone is not an auto-compaction signal.

A context-length error near `664K / 1.0M` is not a hidden `500K` limit and does not by itself show a broken counter. With a `1,048,576` context window and `max_completion_tokens = 384000`, the provider can reject as soon as the messages exceed `664,576`, because it validates the full requested completion allowance too. For example, `664,712 + 384,000 = 1,048,712`, which exceeds the window by 136 tokens even though message utilization is only about 63%. The automatic budget reserves that output plus a 5% margin and limits an 85% preference to 58%, so subsequent turns compact earlier. If the correct capacity was not known before this request, hellogrok still recognizes the exact structured rejection, changes only the outgoing completion allowance to `maximum context - message tokens`, and retries once without falsifying displayed usage.

A display above 100% during an active tool loop has a different cause. Grok Build performs its normal threshold check before sampling, but after a tool result the loop's separate preflight check triggers only when the estimate is already greater than the context window. Large tool output can therefore move the live estimate from below the threshold to well over 100% before that guard runs; compaction then reduces the same active context. This is an estimated live-context value, not accumulated billing usage.

Older hellogrok builds filled missing provider usage with zero-valued fields. That made a syntactically valid response report `total_tokens: 0`, so Grok Build repeatedly reset its baseline to zero and never reached the configured threshold. Current builds preserve trustworthy provider totals, derive a total only when complete input and output measurements exist, and emit `usage: null` for missing, partial, negative, fractional, all-zero placeholder, or otherwise untrustworthy accounting. A native Responses measurement with positive complete `context_details` remains usable even if its separate billing counters are zero.

For first-party DeepSeek, hellogrok also requests streaming usage and preserves the provider's billing `total_tokens`. Capacity metadata follows one rule across DeepSeek, GPT, Claude, Grok, Gemini, and other channels: an explicit `[model.*]` or inherited `[model_providers.*]` value is authoritative; without one, valid upstream `X-Grok-*` headers are passed through and learned. A context rejection may also disclose the window through `context_window`, `max_context_tokens`, `maximum_context_length`, `maximum_context_tokens`, `model_context_window`, or `max_model_len`; hellogrok accepts only one positive, unambiguous, non-overflowing value. Provider-only configured `max_completion_tokens` and trustworthy learned `context_window` values are temporarily projected onto `[model.*]` where current Grok Build inheritance or response-header downgrade behavior would otherwise use the wrong value. The terminal usage updates the numerator. Grok Build computes the trigger from the full denominator, so hellogrok temporarily lowers each unsafe model threshold with the documented output reserve and margin.

Current Grok Build deliberately ignores a context-window response-header downgrade. Forwarding the header alone therefore cannot correct a catalog or fallback denominator, but hellogrok now places a trustworthy learned window into the temporary model configuration and reloads open sessions. This can only happen after the provider exposes the value, so explicit `context_window` and `max_completion_tokens` remain the only predictable first-turn setup. A relay that omits both capacity and usage cannot be made exact without provider metadata, a tokenizer, and hidden prompt overhead. `usage: null` deliberately preserves Grok Build's prior baseline instead of corrupting it.

This is why the built-in `grok-4.5` display is normally correct: Grok Build's model catalog supplies its 500,000-token window and model-specific 80% threshold, while the native xAI Responses stream supplies the live-context `context_details` expected by the sampler. An unknown custom model instead falls back to 200,000 tokens unless configured or discovered, and older hellogrok versions could reset its live baseline to zero. There is no generic hidden 500K cutoff.

If a completed or restored session is already at its effective threshold, the next user prompt triggers Grok Build's pre-sampling auto-compaction before any provider request. hellogrok's process-level test covers that resumed-session path through Responses, Messages, and Chat Completions. A large tool result during an active turn can still cross the threshold between checks; the next loop guard then compacts that same active context.

Complete input/output usage gives an exact baseline for ordinary DeepSeek requests. Server-side Web Search is the one semantic boundary: DeepSeek's official Responses documentation exposes no `context_details` field and does not say whether a multi-step hosted-search response reports the final live context or cumulative billing usage. hellogrok can only project the complete returned counts into Grok Build's extension. Auto-compaction therefore no longer fails because of a zero baseline, but the exact hosted-search trigger point still depends on DeepSeek's wire semantics and requires validation against real API responses. The same rule applies to non-DeepSeek channels: trustworthy terminal usage plus a correct `context_window` supports exact triggering; a provider that omits usage can only retain the prior baseline and grow approximately.

### The Grok Build window stops accepting clicks

First distinguish an unresponsive interface from a long active task. If hellogrok and Grok Build logs continue to show requests, heartbeats, tool calls, or phase transitions, the agent is still running or waiting in the provider queue; DeepSeek may legally keep that request queued for ten minutes. The body-idle limits above now prevent a silent upstream from holding the proxy path forever.

If the whole window truly stops processing input while logs also stop advancing, the HTTP proxy alone cannot establish a UI-thread deadlock. Record the exact timestamp, then preserve the matching Grok Build unified log, hellogrok log, Windows Application Hang/crash event, and a process dump if available. A `504` or `proxy_stream_error` identifies a bounded upstream wait; a renderer/process hang without either requires Grok Build-side evidence rather than a provider-specific network workaround.

### A Claude Messages channel selects the wrong model or returns 404

Use `api_backend = "messages"` (plural). Grok Build defines only `chat_completions`, `responses`, and `messages`; hellogrok rejects the obsolete singular spelling. `base_url` must be the API root before `/messages`: for an endpoint at `/v1/messages`, configure a URL ending in `/v1`. A capable Messages channel is temporarily shown to Grok Build as Responses but still calls that upstream `/messages` endpoint. Also ensure `model` is the provider's actual upstream model ID rather than the channel ID.

### An open window did not follow the proxy switch

Check the **Grok session hot switch** line in **Status and logs**. Automatic switching applies to idle custom-model sessions on a shared leader and supports both current and legacy ACP model-switch method names. On Windows, a live named-pipe leader misreported as stale by Grok Build 1.0.x is accepted only when its leader lock is actively held. A working or input-blocked session is skipped safely; reselect its current model in `/model` after the active operation finishes. A window started with `--no-leader` exposes no external IPC to hellogrok and also requires manual reselection or a new window.

### A model switch asks to start a new conversation

Grok Build replays all historical reasoning items, including provider-encrypted state, after `/model` changes. hellogrok records the emitting signature domain and removes only known foreign encrypted reasoning from the target request; normal messages, tool calls, tool results, search history, and unencrypted reasoning remain unchanged. For an older conversation whose opaque state predates the local provenance index, hellogrok first preserves the request and performs one clean replay only if the upstream returns a structured signature or decryption rejection. A repeated deterministic rejection is marked non-retryable instead of entering Grok Build's generic retry loop.

### The configuration still points to localhost after a forced exit

Ensure no hellogrok process is running, then execute `hellogrok restore`. Do not run `restore` against an active proxy.

### Proxy stop is deferred after editing the active configuration

hellogrok merges proxy-managed fields individually during shutdown, so changing `supports_backend_search` or leaving an unrelated TOML setting unfinished does not need to be rolled back before exit. A normal proxy stop is deferred only when recovery still finds an unowned `127.0.0.1:18787` route, the configuration file cannot be accessed, or another manager owns it. Restore the model header or replace the temporary URL with the intended upstream URL for a clean stop. Tray **Exit** always terminates and leaves unresolved recovery state for the next launch.

### Port `18787` is already in use

Stop the process that owns `127.0.0.1:18787` before enabling the proxy. hellogrok claims the port before changing Grok configuration and shows a startup error if it is unavailable; it does not silently switch ports because the rewritten channel URLs and recovery state must agree on one address.

### Autostart works, but a channel has no credentials

Move shell-only environment variables into the persistent user or service environment, then restart the login service. The autostart process cannot inherit variables that existed only in an earlier terminal session.

### A provider manager prevents a clean proxy stop

Open the provider manager (for example, CC Switch) and disable its Grok Build takeover before stopping hellogrok. This preserves the intended restoration order. If immediate termination is required, tray **Exit** still closes hellogrok and retains the pending recovery transaction.

## Development

Run the local quality checks:

```bash
go test ./... -count=1
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Windows users with configured live channels can run the integration smoke tests:

```powershell
.\scripts\run_grok_all_channels_test.ps1
.\scripts\run_grok_all_channels_test.ps1 -RequireWebSearch -MaxTurns 1 -TimeoutSeconds 150
.\scripts\run_grok_all_channels_test.ps1 -RequireSubagentSearch -MaxTurns 4 -TimeoutSeconds 240
.\scripts\run_grok_all_channels_test.ps1 -RequireWebFetch -MaxTurns 2 -TimeoutSeconds 150
```

CI runs tests and default builds on Windows, Linux, Intel macOS, and Apple Silicon macOS. It also builds the optional tray target natively on Linux and macOS. Tagged releases produce amd64 and arm64 artifacts for all three operating systems.

## Limitations

- hellogrok cannot create provider-side search capability. A hosted-search channel must actually support search and return its results.
- Responses-to-Messages/Chat conversation conversion is enabled only for capability-enabled channels (explicit `supports_backend_search = true`, a selected default search model, the first-party DeepSeek endpoint default, or a hosted-search request resolved from Grok Build's remote model catalog), plus Grok Build's fixed non-streaming WebSearchClient request. Other cross-protocol requests are rejected.
- A relay that removes tool declarations, tool calls, citations, or result events cannot be fully repaired downstream.
- A provider that ignores `stream=true` cannot be made truly streaming after its complete JSON response has already arrived; hellogrok logs and uses a buffered compatibility fallback.
- Provider-encrypted hidden reasoning is scoped to its emitting signature domain. Cross-domain switching preserves visible conversation and tool history but intentionally omits incompatible private reasoning.
- Provider-specific API extensions outside the supported Responses, Chat Completions, and Messages formats may require additional adaptation.
- Upstream availability, model access, account pools, rate limits, and gateway errors remain the provider's responsibility.
- Optional Unix tray behavior depends on the installed desktop environment; the standard Unix CLI is the portable path.
- Current release artifacts are unsigned. Build from source when local trust requirements demand it.

## Contributing

1. Create a focused branch for the change.
2. Follow the existing package boundaries and avoid unrelated refactors.
3. Add or update tests for behavior changes.
4. Run the quality checks above.
5. Update both README files when user-facing behavior changes.
6. Open a pull request describing the problem, approach, and verification results.

## License

Licensed under the [MIT License](./LICENSE).
