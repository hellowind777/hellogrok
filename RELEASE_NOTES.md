# Release Notes — v0.1.5

## Each channel keeps its real upstream protocol

hellogrok now exposes channel-scoped `responses`, `messages`, and `chat/completions` endpoints through one protocol-aware facade. Ordinary conversations continue to use the channel's configured backend. A channel with `supports_backend_search = true` is projected to Grok Build as Responses only where hosted tools require it, while hellogrok translates the request and stream at the provider boundary.

Responses search passes through unchanged. Messages receives its native hosted-search tool, and Chat Completions uses `web_search_options`, `search_parameters`, or an explicit Responses/Messages bridge selected by `chat_search_dialect`. The result is converted back into Grok Build's native `web_search_call`, citation, and source-count representation.

## Any supported format can be the configured search model

A channel selected through `[models].web_search` or `GROK_WEB_SEARCH_MODEL` can execute Grok Build's client `web_search` even when `supports_backend_search` is false or omitted. hellogrok recognizes the dedicated WebSearchClient request and sends it through that channel's real Responses, Messages, or Chat search API. If no search model is selected, Grok Build retains its authenticated official fallback.

The adapter accepts a result only when the provider confirms that search completed and returns non-empty answer text. It preserves real source URLs and citations so Grok Build can render its native deduplicated site count; a relay that ignores its search extension is reported as an upstream error rather than a successful search.

## Tool history remains valid across protocol conversion

Before forwarding, hellogrok checks the pairing rules of all three protocols. Messages `tool_use` batches must be resolved by leading `tool_result` blocks in the immediately following user message, and parallel Responses function calls are grouped into one adjacent Messages assistant/result pair during conversion. Missing results return a non-retryable `400` instead of reaching the provider and entering Grok Build's retry loop.

Messages-compatible streams that omit the required empty `signature` on a thinking-block start are completed without modifying later provider `signature_delta` values.

## Stalled relays now terminate explicitly

hellogrok waits up to 180 seconds for upstream response headers and up to 180 seconds between SSE reads, while active streams have no total deadline. Heartbeats renew the idle window. A response-header timeout returns a retryable `504`; once a stream has started, an idle timeout produces the receiving protocol's stream error and closes the upstream connection.

The proxy also records the model declared by raw Responses, Messages, and Chat Completions output before normalization. Logs identify silent model substitution and conflicting declarations without changing routing or response content.

## Configuration and channel identity stay isolated

Configuration recovery now records the provider's real protocol separately from the backend projected to Grok Build and restores earlier recovery transactions. The old process-local search replay cache has been removed; search history is reconstructed from the current request and verified sources instead.

Channel-owned credentials and extra headers remain isolated from Grok login authentication. Invalid header declarations and attempts to replace proxy-controlled request headers are rejected before routes are activated.
