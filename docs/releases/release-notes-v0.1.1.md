# Binlog Server v0.1.1

Binlog Server `v0.1.1` is a focused operator-experience update for the embedded web console. It refines the day-to-day UI flow for task operations, improves in-app handling for API `401` responses, makes failed upload retry easier from the task detail view, and keeps the release operationally light with no schema migration.

## Highlights

- Refreshed the operator-focused web UI to make routine monitoring and task actions easier to navigate
- Improved in-app guidance for API `401` authentication failures
- Added a retry-upload action in the task detail workflow for failed upload attempts
- Added Playwright-based frontend E2E coverage for the updated UI paths
- Kept the upgrade lightweight with no schema migration required for `v0.1.1`

## What's Included

This release includes the following operator-visible changes:

- A more alert-first dashboard and task-detail workflow for the embedded operator console
- In-app recovery guidance when API auth returns `401`
- A retry-upload entry in the task detail workflow for failed uploads

## Upgrade Notes

`v0.1.1` is intended to be a straightforward upgrade from `v0.1.0`.

Please confirm the following before rollout:

- No database schema migration is needed for this release
- If API auth is enabled, verify the deployed frontend and backend auth settings stay aligned
- If you rely on upload workflows, validate the retry-upload action against your environment after deployment
- If you package static UI assets separately, ensure the refreshed frontend bundle is deployed together with the `v0.1.1` binary
- If you use browser-based operational workflows, include the updated UI paths in your release verification checklist

## Known Limits

- This release does not change the project’s core replication or upload semantics
- Retry-upload remains a manual recovery path rather than an automatic remediation mechanism

Chinese release note is available here:
https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.1.1.zh-CN.md
