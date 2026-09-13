# Release Notes — v0.1.30

## Hosted web search now negotiates closed Responses schemas instead of assuming tolerance

- **The standard source hint stays the default, and closed enums are proven rather than guessed.** Every Responses hosted-search request still asks for `web_search_call.action.sources` — the OpenAI-documented `include` value that carries the full consulted-URL list behind Grok Build's native site count. Providers that implement or ignore the hint keep maximum fidelity. A provider whose `include` is a closed enum no longer breaks the search: hellogrok now negotiates the deviation from evidence instead of from a provider allowlist.

- **Known closed-schema hosts are handled ahead of time.** Volcengine Ark (`*.volces.com`) rejects unknown `include` values with `InvalidParameter` and rejects hosted-search declarations carrying fields outside its closed tool schema (for example Build's `filters` domain filter, or the `allowed_tools` selector shape). For those hosts hellogrok omits the hint and sends a sanitized declaration: unknown tool fields are stripped to the documented set (`type`, `max_keyword`, `limit`, `sources`, `user_location`), and a pure hosted-search `allowed_tools` selection collapses to the equivalent `required` selector. Source display for such hosts falls back to the URLs in their response annotations, which hellogrok already normalizes into both Grok Build render paths.

- **Unknown strict providers self-heal in one replay, with no retained state.** Any other upstream that proves its schema closed with a `400` — an include rejection or a tool-declaration rejection — triggers a single in-request rewrite-and-replay: strip the include list, or sanitize the declaration, then resend the same request. The rewrite is single-shot per request, so it cannot loop; a rejected request never executed a search, so the replay is side-effect free and consumes no billed search quota. No per-host, per-channel, or on-disk memory is kept: the negotiation is a property of the wire exchange, so relays, new hosts, and future providers converge without a hellogrok update.

- **Citation entries that omit parser-required indices no longer fail the whole response.** Grok Build's annotation parser requires `start_index`/`end_index` on `url_citation` entries, while some Responses providers return citations with only `url`, `title`, and metadata. Missing indices are now filled with `0` on every route, in non-streaming bodies and streaming terminal events alike. The cost is citation highlight precision; the alternative was a rejected search response.

## Verified end to end against a live closed-schema provider

The negotiation was exercised against Volcengine Ark's Doubao search as a configured Grok Build `web_search` backend: the direct API rejects the hint and the unsanitized declaration with `400 InvalidParameter`, while the negotiated request returns `200` with a completed `web_search_call` and annotation-backed sources (`search evidence declared=true calls=1 completed=1 sources=5 annotations=5` in the proxy log). New tests pin each behavior: include rejection replays once without the hint, tool-declaration rejection replays once with the sanitized declaration, index filling applies to every route, and compliant relays keep their include entries and selector untouched.

Every change stays inside the existing safety contract: rewrites happen before Grok Build's strict validators see the wire, deterministic refusals remain non-retryable, and no new configuration surface was added — the negotiation is automatic and invisible unless a schema deviation is logged.

Restart both hellogrok executables after upgrading.
