# Release Notes — v0.1.36

## Text-only channels no longer end a turn on `vision_not_supported`

- **What you saw.** When a conversation carried image content — for example a `read_file` of a picture, whose result Grok Build renders as image blocks — every later request forwarded those image parts to the upstream. A text-only model then rejected the whole request with `400 vision_not_supported` ("The request model is not multimodal … does not support image input"), and the turn died on the red error.
- **What changed.** hellogrok now recognizes that rejection (`400` with a `vision` error code, or a `not multimodal` / `does not support image` message) on any of the three upstream protocols. It replaces every image content part (`image_url`, `input_image`, `image`, including images inside tool results) with a one-line text placeholder that tells the model the visual payload was omitted, and retries the request once. The turn continues without the images instead of failing. Channels are deliberately not remembered as text-only, so the decision is re-derived from each upstream rejection and a channel whose upstream later gains vision support keeps working unchanged.

## Third-party tool calls with wrong or missing arguments are normalized or dropped before dispatch

- **What you saw.** Third-party models occasionally emit Grok Build parameter names from their own training prior — `read_file` called with `target_path` instead of `target_file`, for instance — and Grok Build's strict schema validation answered `Failed to parse arguments for tool …: missing field …`, marked the call failed in the TUI, and burned a round trip. Rarer relay/model glitches emit a tool call whose argument fragments never arrive at all; the empty arguments object then fails the same validation with a guaranteed `missing field` error.
- **What changed.**
  - Parameter aliases are normalized against the tools declared on the request: `target_path` now maps to `target_file`, `target_directory`, or `file_path` (whichever the declared schema actually has), joining the existing alias table. Rewrites only apply to properties the tool declares, so unrelated JSON is never touched.
  - A streamed call that accumulates no argument fragments at all is discarded before Grok Build dispatch when the declared tool requires properties — an empty object can never satisfy it — and the defect stays visible as an `empty-args-discarded(name=…)` proxy log note. Calls to tools without required properties keep their empty arguments, which is the valid zero-argument convention.

## Inline reasoning no longer leaks into the visible reply

- **What you saw.** Thinking models emit several chain-of-thought phases per turn, and some relays misroute a reasoning tail into `content` with only the closing tag. The previous think-tag stripper only handled a `<think>>…</think>` block at the very head of the answer, so second-phase spans, unbalanced `</think>` tails, and stray or delta-split closing tags appeared verbatim in the reply bubble.
- **What changed.** The Chat think-tag state machine now strips spans wherever they appear in the stream: text before an opening tag is emitted immediately, the span is buffered until its closing tag and routed to the reasoning channel, an unbalanced closing tag at the head of a turn classifies the text before it as reasoning, and stray or delta-split closing tags are removed or held instead of being shown. The non-streaming path peels every span from the answer content. Reasoning that arrives after visible reply text is still dropped by design, because Grok Build renders reasoning only as a prefix Thought.

Restart both hellogrok executables after upgrading.
