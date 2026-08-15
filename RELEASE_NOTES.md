# Release Notes — v0.1.9

## DeepSeek Web Search preserves Grok Build source details by default

When a first-party `api.deepseek.com` request can execute hosted search and no search-protocol override is configured, hellogrok now uses the official Anthropic Messages API for that search turn. DeepSeek Messages returns structured `web_search_tool_result` URLs that Grok Build can render as native site counts, source domains, and expandable numbered links.

This protocol choice applies only while hosted search is eligible. A request whose `tool_choice` disables search or selects another function stays on the channel's configured Responses, Messages, or Chat Completions API. An explicit `chat_search_dialect = "responses"` or `"messages"` remains authoritative and can bridge any configured backend.

## Responses source discovery is forward-compatible

Every Responses hosted-search request now asks for `web_search_call.action.sources` without replacing or duplicating caller-supplied `include` entries. DeepSeek currently accepts but silently ignores this hint; keeping the standard field means source URLs will flow through automatically if the provider implements it later. hellogrok never invents omitted sources.

DeepSeek Responses `action.queries` values are normalized into Grok Build's singular `action.query` display field while the original array remains available for replay. Messages search results also retain provider metadata such as `title`, `page_age`, and `encrypted_content`, and authoritative provider sources are no longer mixed with citation-only URLs.

## Client search completes with final text

The Grok Build WebSearchClient adapter no longer turns automatic tool choice into a permanently forced server-tool selection. Agentic providers can execute Web Search and then return the final text Grok Build expects, instead of repeating search calls until the turn ends without usable output.

## Bridged search history stays cache-stable

Responses-to-Messages conversion now represents historical backend Web Search calls with the same stable human-readable summary used by Grok Build. It no longer reconstructs synthetic provider server-tool blocks that were never returned by the Messages API. Live calls and results remain provider-native, and native and bridged cache-prefix tests continue to cover Responses, Messages, and Chat Completions.
