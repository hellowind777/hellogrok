# Release Notes — v0.1.22

## Tool-call compatibility

- Fill missing local tool-call IDs in Chat and Messages responses before Grok Build receives them.
- Keep Responses output-item and function-call identities consistent across stream events; reject conflicting identities.
- Assign separate indexes to parallel tool calls when an upstream JSON response must be delivered as Chat SSE.
- Repair existing Chat history only when a missing call ID can be associated with exactly one result. Ambiguous history still requires a new session.

## OpenCode Go and Zen

- Preserve identity from the original request across Responses, Messages, Chat, search conversion, and internal retries.
- Extend `x-opencode-session` projection to official Go and Zen routes. Requests without client identity receive an isolated operation ID instead of a local missing-identity rejection; retries reuse it. This does not restore conversation routing or cache affinity across separate requests.
- Prefer explicitly configured User-Agent headers, then the incoming client identity. If both are absent, use Grok Build identification derived from the installed `grok --version`.

## Windows

The status/log window title now includes the running hellogrok version.

Restart hellogrok after upgrading. Provider-hosted search and other provider capabilities still require upstream support; these fixes do not guarantee every third-party model supports every Grok Build tool.
