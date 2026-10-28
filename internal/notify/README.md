# internal/notify Module

Generic webhook for ADR 0009 decision 1. One JSON POST. No chat-vendor clients.

## Files

- `webhook.go`: `Webhook.Post` sends `{version, events[]}`. `X-BinlogServer-Token` is set when a token is configured. `X-BinlogServer-Delivery` is the SHA-256 of the body so a retry is the same delivery. Transport errors, 5xx and 429 retry with exponential backoff. Other 4xx do not. Redirects are not followed, so the token is not sent to another host. The response body is drained and closed after the status check.
- `webhook_test.go`: httptest coverage for a delivered body, a 500 then 200 retry, a 400 that does not retry, and a 302 to another host that does not receive `X-BinlogServer-Token`.

## Exports

- `Event` — one health transition or one finding transition.
- `Webhook.Post(ctx, events)` — delivers the batch or returns the last error.

## Dependencies

- Upstream: `internal/tasks` (the evaluator is the only caller) and `internal/app` (builds it from `notify.*`).
- Downstream: `net/http`.

## Update Rule

- Payload fields, headers, or retry behavior change here and in `docs/guide/admin/observability.md`.
