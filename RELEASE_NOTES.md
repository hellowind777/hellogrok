# Release Notes — v0.1.37

## Reasoning that discusses think tags no longer leaks into the visible reply

- **What you saw.** v0.1.36 stripped inline think spans wherever they appeared in the stream, but it treated every literal tag occurrence as a delimiter — including tags a model wrote as quoted prose. Reasoning that discussed the tags themselves (for example analyzing a thinking-leak defect) terminated its own span at the first quoted `</think>`, and the rest of that reasoning leaked into the visible reply. Quoted tags inside visible text were stripped as well, so replies citing the tags arrived mangled. A related streaming-path defect: the non-streaming content peeler classified unbalanced closing-tag tails and removed stray closing tags on *every* SSE delta, consuming a closing tag before the turn-level state machine ever saw it, which could flash split tags in the reply.
- **What changed.** A tag occurrence wrapped in backticks or double quotes is now treated as prose about the tags, not as a stream delimiter: quoted tags never open or close a span, only unquoted stray closing tags are removed, and quoted tags inside visible text reach the client intact. The unbalanced-tail classification and stray-close removal in the content path now apply only to complete message objects (those carrying a role); streaming deltas leave classification to the turn-level state machine as designed.

Restart both hellogrok executables after upgrading.
