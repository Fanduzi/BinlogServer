# Binlog Server v0.3.0

Binlog Server `v0.3.0` hardens the rate-limiting layer with an upload token-bucket limiter and a bug fix for LRU eviction ordering, and extends the CI e2e matrix with tiered job profiles to cover meta-failover scenarios.

## Highlights

- Added `UploadRateLimiter` (token-bucket) in `internal/upload` to throttle outbound object-storage upload operations and prevent burst exhaustion of bandwidth or API quotas. `NewUploadRateLimiter(uploadsPerSecond, burst)` accepts `<=0` for unlimited; `Wait(ctx)` respects context cancellation. Verified race-free with 8 unit tests.
- Fixed LRU eviction in `IPRateLimiter`: `lastSeen` was only updated on the slow path (entry creation), causing the LRU to evict the oldest-created IP rather than the least-recently-used one. Switched to `atomic.Int64` updated on every read- and write-path access.
- Restructured the e2e CI matrix into three tiers: PR runs `quick + retry-upload`; push to `main` adds `meta-failover`; manual dispatch allows any profile. `cluster-roles`, `observability`, and `worker-crash-recovery` jobs are now dispatch-only.

## Upgrade Notes

- No schema migration is required for `v0.3.0`.
- No breaking backend API contract change is introduced in this release.
- `UploadRateLimiter` is not yet wired to the upload pipeline; no configuration changes are needed at this time.

## Operator Impact

- The LRU fix ensures that IP rate-limiter entries are evicted by actual last-access time, improving fairness under high IP-diversity traffic.
- Upload rate limiting infrastructure is in place for future pipeline integration.
- CI now automatically validates meta-failover on every push to `main`, providing earlier signal on cluster-level failures.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.3.0.zh-CN.md
