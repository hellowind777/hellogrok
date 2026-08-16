# Release Notes — v0.1.17

## Custom channels retain their identity across `/resume`

Every proxied custom channel now uses its model-table ID as the runtime identity seen and persisted by Grok Build. The configured `model` remains the upstream wire model and is restored byte-for-byte when the proxy stops.

Sessions can therefore resume through the channel that created them even when several custom channels and an official model share the same upstream model name, such as `grok-4.6`. Historical summaries that already contain only the shared model name require one manual model reselection.

## Provider errors remain actionable

hellogrok now preserves upstream error status and body while deriving retry behavior from structured error codes and messages. Authentication, permission, billing, insufficient balance or quota, invalid request, and invalid model failures are non-retryable; rate limits, timeouts, overload, and temporary service failures remain retryable. An explicit upstream `X-Should-Retry` header takes precedence.

This allows billing and account failures from compatible relays to reach the conversation instead of being hidden behind generic retries.

## Stopped sessions receive a clear diagnostic

An ordinary proxy stop keeps a diagnostic listener for stale sessions and returns a structured, non-retryable `proxy_stopped` response that asks the user to reselect a model. Tray **Exit** always closes the listener and releases the local port after attempting configuration recovery, including when cleanup must be deferred.
