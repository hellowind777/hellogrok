# Release Notes — v0.1.41

## Self-healing path for unnamed tool calls

- **Unnamed Chat tool calls no longer die as a silent terminal `NotFound`.** When a streamed call keeps an empty name after shape inference (for example an empty name plus arguments missing the opening `{` with trailing garbage), hellogrok now routes it to a deterministic carrier instead of handing Grok Build an empty name the model never sees fed back.
- **Parseable arguments go to `run_terminal_command` (`command` carries the raw text); corrupt arguments go to `read_file` (`target_file` carries the raw text).** Both carriers fail cleanly, so the raw arguments return through a `tool_result` and the model regenerates the call on the next turn. The route is logged as `unresolved-name-routed(name=…)`. When neither carrier is declared on the request, the call is left untouched.

Upgrade the running proxy to pick up the new behavior; the next affected turn recovers without a new session.
