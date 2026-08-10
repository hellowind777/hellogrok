<p align="center">
  <img src="./assets/hellogrok.png" alt="hellogrok icon" width="128">
</p>

# hellogrok

A cross-platform local proxy that makes Grok Build custom model channels work with common API formats, native Web tools, isolated authentication, and automatic configuration recovery.

[![Version](https://img.shields.io/badge/version-0.1.7-2f6feb.svg)](./internal/appinfo/appinfo.go)
[![Go](https://img.shields.io/badge/Go-1.26.5-00ADD8.svg)](./go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![Platforms](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey.svg)](#platform-support)
[![LINUX DO](https://img.shields.io/badge/LINUX_DO-recognized-0A84FF?logo=linux&logoColor=white)](https://linux.do)

[English](./README.md) · [简体中文](./README_CN.md) · [Release notes](./RELEASE_NOTES.md) · [Changelog](./CHANGELOG.md)

> 🏅 This project is linked & recognized by the [LINUX DO](https://linux.do) community.

## Contents

- [Why hellogrok](#why-hellogrok)
- [Features](#features)
- [Search and configuration](#search-and-configuration)
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
- When `supports_backend_search = true`, temporarily exposes the channel to Grok Build as Responses, then translates requests, responses, and SSE events to and from the provider's real protocol. This lets Grok Build consume `web_search_call`, sources, and site counts for every supported upstream format.
- When `supports_backend_search = false` or is omitted, keeps Grok Build on the configured native consumer so its client `web_search` uses `[models].web_search`, `GROK_WEB_SEARCH_MODEL`, or the authenticated official fallback.
- Exposes channel-scoped `/responses`, `/messages`, and `/chat/completions` routes and restores the original `api_backend` byte-for-byte when the proxy stops.
- Validates protocol tool history before forwarding: Responses calls require matching `function_call_output` items, Messages `tool_use` blocks require `tool_result` blocks in the immediately following user message, and Chat tool calls require matching tool messages. Deterministic failures return a non-retryable `400` instead of entering Grok Build's retry loop.
- Converts provider-private `keepalive`, `keep-alive`, `keep_alive`, `heartbeat`, and `ping` frames into standard SSE comments before they reach Grok Build, without consuming Responses sequence numbers, and closes each upstream stream as soon as its protocol terminal event arrives.
- Bounds the wait for upstream response headers and gaps between SSE data at 180 seconds without imposing a total deadline on long-running model streams. Every upstream byte, including a normalized heartbeat, renews the stream idle window.
- Logs the model declared by the raw upstream response before normalization, including terminal-frame precedence, case-insensitive mismatch detection, and conflicting declarations, without changing routing or response data.
- Completes a missing empty `signature` on Messages `thinking` block starts while preserving the provider's later `signature_delta`, so Messages-compatible relays remain consumable by Grok Build's strict native decoder.
- Preserves each configured upstream URL path and model identifier.
- Prepares every explicit custom channel before use, avoiding first-request failures after `/model` switching.
- Preserves portable conversation history during model hot switching while withholding only encrypted reasoning known to belong to a different channel, protocol, wire model, or upstream endpoint.

### Native Web tools

- Supports Grok Build's native `web_search` workflow for hosted and client-search modes.
- With `supports_backend_search = true`, every supported format uses the current channel's own hosted search: Responses passes through, Messages receives `web_search_20250305`, and Chat uses its configured search dialect or protocol bridge.
- The Grok Build side always receives canonical Responses search events for a capable channel, including completed `web_search_call` items, verified sources, citations, usage, and the native deduplicated site count. Responses providers are asked for complete `web_search_call.action.sources` metadata while existing `include` entries are preserved; provider-level citations and Chat annotations/search-result containers are normalized into the same canonical source fields.
- With `supports_backend_search = false`, Grok Build uses client `web_search` instead. Any of the three backends can still be selected as that client-search model; the proxy adapts only Grok Build's fixed non-streaming WebSearchClient request.
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
- On normal stop, tray exit, Ctrl+C, SIGTERM, or failed startup, restores untouched temporary values while preserving concurrent user edits through a field-level three-way merge.
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
| `[models].web_search` or `GROK_WEB_SEARCH_MODEL` is set | Grok Build client `web_search` uses the selected model. That model may use `responses`, `messages`, or `chat_completions`; its WebSearchClient request is handled independently of `supports_backend_search`. The environment variable takes precedence, and selection is resolved without a startup request. |
| Any channel with `supports_backend_search = true` | Grok Build uses Responses hosted tools while hellogrok calls that channel's own working search API: Responses, Messages `web_search_20250305`, or the selected Chat search dialect/bridge. |
| Any channel with `supports_backend_search = false` or omitted | Grok Build uses client `web_search`: first `[models].web_search` or `GROK_WEB_SEARCH_MODEL`, otherwise its authenticated official fallback. |
| No usable hosted or client search path | `web_search` is unavailable for that model. |
| `web_fetch` | Remains independent of the search-model selection and follows the active tool permissions. |

hellogrok never creates, selects, or replaces `[models].web_search`, and startup sends no search-capability probes. A Messages channel marked capable must actually support `web_search_20250305`. Chat defaults to `web_search_options`; official DeepSeek Chat bridges to its Messages API, and official xAI Chat bridges to Responses. Set `chat_search_dialect` to `web_search_options`, `search_parameters`, `messages`, or `responses` when a relay needs an explicit strategy. A provider without the selected capability cannot be made searchable by the proxy.

For client search, hellogrok clarifies tool descriptions but never infers or forces tool use from prompt text. Mandatory selection comes only from structured `tool_choice`. Internal wire aliases are changed only in protocol-defined tool declarations, choices, and call-name fields; response text, URLs, tool arguments, tool results, and other business JSON remain unchanged.

### Example configuration

This example uses a Chat Completions channel that supports `web_search_options` as Grok Build's client-search model:

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

For any channel that should use its own hosted search in ordinary conversations, set `supports_backend_search = true` only after confirming the selected provider API supports it. Messages uses `web_search_20250305`. Official DeepSeek Chat automatically uses `/anthropic/messages`; other Chat relays can select an explicit `chat_search_dialect`.

### Supported channel settings

| Setting | Required | Default | Purpose |
|---------|----------|---------|---------|
| `model` | No | Model table ID | Model identifier sent to the upstream channel. |
| `base_url` or `api_base_url` | Yes | None | Custom upstream endpoint. Models without a custom URL are not proxied. |
| `api_backend` | No | `chat_completions` | Native upstream API format: `responses`, `messages`, or `chat_completions`. A capable non-Responses channel is temporarily projected as Responses only to Grok Build; the upstream format and restored config remain unchanged. |
| `chat_search_dialect` | No | Host-based | Chat hosted-search strategy: `web_search_options`, `search_parameters`, `messages`, or `responses`. Defaults are `messages` for official DeepSeek, `responses` for official xAI, and `web_search_options` elsewhere. |
| `api_key` | One auth method | None | Static channel credential. Prefer `env_key` for shared configurations. |
| `env_key` | One auth method | None | Environment variable name or ordered list of names containing the channel credential. |
| `auth_provider` | One auth method | None | Grok command-based authentication provider. |
| `auth_scheme` | No | `bearer` | Upstream authentication scheme. Set `x_api_key` only for providers that explicitly require `X-Api-Key`. |
| `extra_headers` | No | Empty | Additional channel-owned HTTP headers, including provider-specific authentication. Proxy-controlled framing, content, and connection headers are rejected; names are case-insensitive. |
| `env_http_headers` | No | Empty | HTTP headers populated from environment variables. Resolved values use the same header rules as `extra_headers`. |
| `supports_backend_search` | No | `false` | When true, uses this channel's own hosted search and exposes canonical Responses search events to Grok Build for all three upstream formats. When false, Grok Build uses its configured or authenticated client-search route. |

Model settings may be declared directly under `[model.<id>]` or inherited from a referenced `[model_providers.<id>]`. Model-level values take precedence.

Quote the full ID when a channel ID contains a dot, for example `[model."provider.v1-beta"]`. Dashes do not require quoting. A value such as `name = "Provider.v1-beta"` is display-only and may use dots or dashes directly. While active, hellogrok also accepts a legacy unquoted dotted header and restores the original text on stop.

Do not manually set a custom channel URL to hellogrok's local address. The application manages temporary local URLs only while the proxy is active.

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
- Go **1.26.5** when building from source.

Grok Build **1.0.0** is the current verified baseline. Newer versions should be checked with the included smoke tests because Grok Build's custom-model behavior may evolve.

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
- **Exit** — restores the configuration, stops the proxy, and exits. Defers when a config-ownership conflict exists.

Only one tray instance runs in a login session; launching it again exits immediately instead of creating a second tray. The remembered tray state is independent from the foreground `hellogrok start` command.

On Windows, the divider in **Status and logs** contains a retention selector and log search. Retention counts distinct dates on which hellogrok actually wrote logs rather than elapsed calendar days; the default keeps the latest 7 usage days, with `off`, 3, 7, 14, and 30 available. Cleanup runs at the next application start. Repeated **Search** clicks move to the next match and wrap to the beginning. Status text wraps; raw log lines remain unwrapped for reliable scanning.

**Quit protection**: When a provider manager still owns Grok Build's configuration, the tray defers exit to avoid leaving an orphaned proxy route — resolve the configuration conflict first, then quit.

Configuration edits made while the proxy is active are merged field by field during shutdown. Values still matching hellogrok's temporary projection are restored to their startup values, while user-edited values and deleted model channels are preserved. Shutdown remains deferred when the merged configuration would leave one of the current takeover's temporary hellogrok routes behind or cannot be parsed safely.

### Compatibility with CC Switch

CC Switch and hellogrok can run at the same time only when CC Switch is not managing Grok Build. CC Switch's Grok Build proxy takeover and provider switch both write `~/.grok/config.toml`; using either operation while hellogrok owns that file creates a configuration-ownership conflict even though the proxies listen on different ports.

- hellogrok refuses to start when it detects CC Switch's Grok Build takeover marker (`PROXY_MANAGED` on its `/grokbuild/v1` route).
- If CC Switch takeover is enabled after hellogrok starts, hellogrok refuses to stop or exit until CC Switch releases Grok Build. This keeps CC Switch from later restoring a stopped `127.0.0.1:18787` route.
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

At startup, hellogrok validates each custom channel and points its URL to a channel-scoped local route. Channels without backend search keep their configured Grok Build consumer and native stream format. Capable channels are temporarily projected as Responses because that is the Grok Build consumer that serializes hosted tools and renders structured search results; hellogrok then translates at the provider boundary and restores the original configuration on stop.

Responses providers remain Responses. Messages providers receive Messages requests and return Responses events through a bidirectional converter. Chat providers use `web_search_options` or `search_parameters`, or bridge to Messages/Responses when configured. Grok Build's fixed non-streaming `WebSearchClient` request uses the same provider adapters when a custom channel is selected as the client-search model.

Native `web_search`, `web_fetch`, official Grok login behavior, and supported subagent workflows remain controlled through Grok Build rather than being replaced by a separate search service.

## Troubleshooting

### No custom routes are found

Confirm that the intended `[model.<id>]` or referenced provider has a valid `base_url` or `api_base_url`. Official models without a custom URL are intentionally excluded.

### `web_search` is unavailable

Check the startup log for the channel's Build and upstream protocols, then inspect the first failing search request. A capable Responses channel must implement the Responses hosted tool; Messages must support `web_search_20250305`; Chat must support its selected `chat_search_dialect`. A channel marked false instead needs a valid `[models].web_search` / `GROK_WEB_SEARCH_MODEL` selection or usable official xAI credentials. `web_fetch` is independent but can still be removed by active tool permissions.

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

Current Grok Build has two Responses source paths: hosted search reads `web_search_call.action.sources`, while its client `web_search` tool reads URL citations from `output_text.annotations`. Before forwarding Responses hosted search, hellogrok adds `web_search_call.action.sources` to `include` without replacing caller entries, so compatible providers return every consulted URL rather than only cited or opened pages. Responses-level `citations`, Messages search-result/citation blocks, and Chat `annotations`, `citations`, `search_results`, or `web_search_results` are normalized into both Grok Build paths. The WebSearchClient adapter uses the same normalization and, only when search execution is independently confirmed, recovers valid HTTP(S) links from the final answer. A normal answer link does not create a search call. If a provider returns no real URL, search activity can still be shown but a trustworthy site count cannot be fabricated.

### `unknown variant keepalive` or an endless `Waiting for response...`

Upgrade both hellogrok executables to the same current release or build, then restart the proxy. Some relays inject private `keepalive`, `keep-alive`, `keep_alive`, `heartbeat`, or `ping` events into an SSE stream. Grok Build's strict Responses deserializer rejects such JSON events even while upstream generation continues. hellogrok absorbs these names from the SSE `event:` field, JSON `type` or `event`, raw data payloads, and empty-data heartbeat frames, then emits the standards-compatible `: keepalive` comment. A completed Responses event, Messages `message_stop`, or Chat Completions `[DONE]` also closes the upstream request immediately rather than waiting for the provider socket.

The completion log includes `heartbeats=<count>`. If the same error remains while that counter stays zero, confirm that Grok Build is routed through the current proxy with `hellogrok routes`; the provider is likely emitting a different private event name that should be diagnosed from a credential-free stream capture rather than added as a model-specific workaround.

hellogrok waits at most 180 seconds for upstream response headers and at most 180 seconds between SSE data reads. A heartbeat counts as data and renews that idle window. A timeout before response headers returns retryable `504`; after a `200` stream has begun, hellogrok emits the receiving protocol's `proxy_stream_error` and closes the upstream. There is no total request timeout, so an active long-running reasoning stream is not terminated. The `response_model` log line shows the upstream-declared and configured models; `mismatch=true` identifies a relay that silently substituted a model, while `conflict=true` means different response frames declared different models.

### A Claude Messages channel selects the wrong model or returns 404

Use `api_backend = "messages"` (plural). Grok Build defines only `chat_completions`, `responses`, and `messages`; hellogrok rejects the obsolete singular spelling. `base_url` must be the API root before `/messages`: for an endpoint at `/v1/messages`, configure a URL ending in `/v1`. A capable Messages channel is temporarily shown to Grok Build as Responses but still calls that upstream `/messages` endpoint. Also ensure `model` is the provider's actual upstream model ID rather than the channel ID.

### An open window did not follow the proxy switch

Check the **Grok session hot switch** line in **Status and logs**. Automatic switching applies to idle custom-model sessions on a shared leader and supports both current and legacy ACP model-switch method names. On Windows, a live named-pipe leader misreported as stale by Grok Build 1.0.0 is accepted only when its leader lock is actively held. A working or input-blocked session is skipped safely; reselect its current model in `/model` after the active operation finishes. A window started with `--no-leader` exposes no external IPC to hellogrok and also requires manual reselection or a new window.

### A model switch asks to start a new conversation

Grok Build replays all historical reasoning items, including provider-encrypted state, after `/model` changes. hellogrok records the emitting signature domain and removes only known foreign encrypted reasoning from the target request; normal messages, tool calls, tool results, search history, and unencrypted reasoning remain unchanged. For an older conversation whose opaque state predates the local provenance index, hellogrok first preserves the request and performs one clean replay only if the upstream returns a structured signature or decryption rejection. A repeated deterministic rejection is marked non-retryable instead of entering Grok Build's generic retry loop.

### The configuration still points to localhost after a forced exit

Ensure no hellogrok process is running, then execute `hellogrok restore`. Do not run `restore` against an active proxy.

### Stop or exit is deferred after editing the active configuration

Version 0.1.7 and later merge proxy-managed fields individually during shutdown, so an edit such as changing `supports_backend_search` no longer needs to be rolled back. If shutdown is still deferred, the edited TOML is invalid or a renamed/moved model still contains one of the current takeover's temporary `127.0.0.1:18787` routes. Restore a valid model structure or replace that temporary URL with the intended upstream URL, then stop again; do not force-terminate the process while Grok Build still points at it.

### Port `18787` is already in use

Stop the process that owns `127.0.0.1:18787` before enabling the proxy. hellogrok claims the port before changing Grok configuration and shows a startup error if it is unavailable; it does not silently switch ports because the rewritten channel URLs and recovery state must agree on one address.

### Autostart works, but a channel has no credentials

Move shell-only environment variables into the persistent user or service environment, then restart the login service. The autostart process cannot inherit variables that existed only in an earlier terminal session.

### Cannot quit while a provider manager owns Grok Build

Open the provider manager (e.g., CC Switch) and disable its Grok Build takeover first, then quit hellogrok. This prevents CC Switch from later restoring a route to a stopped proxy.

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
- Responses-to-Messages/Chat conversation conversion is enabled only for channels explicitly marked `supports_backend_search = true`, plus Grok Build's fixed non-streaming WebSearchClient request. Other cross-protocol requests are rejected.
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
