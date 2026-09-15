# Release Notes — v0.1.35

## Tool calls whose first stream delta was lost are repaired instead of failing

- **What you saw.** Some third-party relays intermittently omit a tool call's first streamed delta — the frame carrying `function.name` and the opening `{"` of the arguments. Grok Build then dispatched an empty tool name with head-truncated arguments and reported `Agent tried calling a tool that doesn't exist`, feeding a parse error back into the model and burning a retry round (observed repeatedly on GLM-family channels through relays).
- **What changed.** hellogrok now repairs that damage in its shared tool-compatibility layer, for all three upstream protocols (`chat_completions`, `messages`, `responses`), streaming and non-streaming alike:
  - Arguments truncated at the head are restored when prefixing `{"` (or `{`) yields exactly one complete JSON object.
  - Empty names are inferred from the argument key set against the tools declared on that request; the shared `command`+`description` shape, which matches both `run_terminal_command` and `monitor`, resolves deterministically to `run_terminal_command`. Anything still ambiguous is left unrepaired rather than guessed.
  - Persisted history replayed in later requests (including `/resume`) receives the same repair, and the repaired name is backfilled onto the matching tool-result message, so broken records no longer reach the upstream verbatim.

## Out-of-order and name-less tool blocks are held until they can be resolved

- **Chat Completions.** Tool frames are now emitted at the stream terminal (`[DONE]`, stream end, or error frame) instead of at `finish_reason`. Grok Build's chat accumulator reads the whole stream regardless of chunk order, so argument fragments a relay sends after `finish_reason` are merged into complete arguments instead of being silently dropped with tail-truncated parameters. Held inline reasoning still flushes at `finish_reason`, so visible latency of thought and text is unchanged.
- **Messages.** `tool_use` blocks are held between `content_block_start` and `content_block_stop`; the accumulated input JSON resolves the name (and repairs its prefix) before the block is re-emitted as start + one complete `input_json_delta` + stop. Unclosed blocks flush at `message_stop`/`error`.
- **Responses.** `function_call` items are held between `response.output_item.added` and `response.output_item.done`; the done frame's complete item additionally serves as a second source for name and arguments, so a truncated argument stream loses to a complete terminal item. Unclosed items flush at `response.completed`/`incomplete`/`failed`.

## Diagnostics

- Tool-call deltas that arrive after an early flush (error terminals) and are therefore discarded are logged per call index as `late-tool-deltas-discarded(index=N)`, making a relay that emits `finish_reason` before its last argument fragments visible in the proxy log instead of surfacing only as a Grok Build parse failure.

Restart both hellogrok executables after upgrading.
