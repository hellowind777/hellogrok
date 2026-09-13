# Release Notes — v0.1.32

## DeepSeek models no longer get an implicit backend-search default after DeepSeek retired hosted web search

- **The old default now routes `web_search` into a dead end.** hellogrok used to project models on the exact first-party `api.deepseek.com` endpoint as `supports_backend_search = true` when the field was omitted, based on DeepSeek's documented provider-hosted web search. DeepSeek has since retired that capability: the current Responses API documents `web_search` and every other built-in tool type as ignored, and its response format no longer includes `web_search_call`. With the old default, a `web_search` call went to the DeepSeek channel, the endpoint silently answered without executing any search, and Grok Build showed a completed turn with no sources — while the client-search fallback chain (`[models].web_search`, `GROK_WEB_SEARCH_MODEL`, the authenticated official fallback) was bypassed entirely because the channel looked search-capable.

- **An omitted field now behaves exactly like every other provider.** The implicit default is removed. Omission preserves Grok Build catalog behavior uniformly: client `web_search` resolves from `[models].web_search`, `GROK_WEB_SEARCH_MODEL`, or the authenticated official fallback, and if none is available the model reports that web search is unavailable instead of pretending to search. hellogrok no longer writes `supports_backend_search = true` into the active configuration for an omitted DeepSeek channel. Explicit values keep their meaning: `true` is honored as a routing declaration (useful on relayed endpoints that still implement a real search extension; the first-party endpoint silently ignores it), and `false` stays opted out unless that route is selected as the default search model.

- **No other DeepSeek behavior changed.** The `[1m]` Anthropic Messages alias, thinking-mode normalization, the 660-second idle policy covering the documented ten-minute queue, Bearer/`X-Api-Key` authentication, and the Chat-to-Responses search bridge dialect are untouched. Configuration that explicitly set `supports_backend_search` before upgrading behaves identically.

Also reflected in documentation: DeepSeek's 2026-09-10 announcement replaced the V4 Flash generation with V4.1 Flash under the model ID `deepseek-flash` (retired `deepseek-v4-flash` / `deepseek-v4-flash-vision-exp` names still route to it), and V4 Pro service continues. Existing DeepSeek channel configurations need no changes.

Restart both hellogrok executables after upgrading.
