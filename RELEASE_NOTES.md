# Release Notes — v0.1.6

## Responses hosted search now returns complete sources

When a Responses channel uses its own hosted `web_search`, hellogrok now requests the provider's complete `web_search_call.action.sources` metadata. Existing `include` entries are preserved and the source request is added only once, so provider-specific response fields continue to work unchanged.

This supplies the source list that Grok Build consumes for its native deduplicated site count, rather than limiting the interface to pages explicitly cited or opened in the answer.

## Source metadata is normalized across every supported protocol

Responses providers may return citations at the response level, while Chat Completions relays may use `annotations`, `citations`, `search_results`, or `web_search_results`. hellogrok now recognizes these structures in both streaming and non-streaming responses and converts their verified URLs into `web_search_call.action.sources` and `output_text.annotations`.

Responses, Messages, and Chat Completions channels therefore retain their own hosted-search format while producing the same Grok Build search-call, citation, and source-count representation. The same normalization also applies when any of the three formats is selected as Grok Build's client-search model.
