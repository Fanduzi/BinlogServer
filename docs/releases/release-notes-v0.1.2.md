# Binlog Server v0.1.2

Binlog Server `v0.1.2` focuses on frontend operator workflow upgrades. This release introduces shared development mock infrastructure, expands mock scenario coverage for cluster resilience states, and reshapes the web console into workspace-oriented navigation with shareable deep links.

## Highlights

- Added opt-in frontend dev mock mode for Vite (`VITE_USE_MOCK=true`) with shared scenario data reused by Playwright E2E.
- Expanded scenario coverage with degraded cluster, lease-risk, and control-plane-down/worker-running views.
- Reorganized the ops console around workspace routes:
  - `/#/overview`
  - `/#/tasks`
  - `/#/sources`
  - `/#/workers`
  - `/#/alerts`
- Moved task filters and source lookup into their domain pages to reduce single-screen overload.
- Updated left navigation behavior to a docked full-height rail with bottom-docked collapse control.
- Improved lease risk rendering under mock-driven views by using dashboard reference time.

## Upgrade Notes

- No schema migration is required for `v0.1.2`.
- No breaking backend API contract change is introduced in this release.
- If you package frontend static assets separately, ensure the refreshed frontend bundle is deployed together with the `v0.1.2` binary.

## Operator Impact

- Better workflow clarity for daily operations due to page-by-domain organization.
- Easier local UI development and demoing without a fully running backend cluster.
- More realistic resilience-state demonstrations in local and CI frontend acceptance tests.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.1.2.zh-CN.md

