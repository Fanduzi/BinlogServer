# Binlog Server v0.4.1

Binlog Server `v0.4.1` is a patch release focused on stabilizing the frontend audit work: it fixes UI regressions introduced during the console refactor, restores button and tag styling to the `main` baseline, and hardens the Playwright coverage around split views and retry-upload behavior.

## Highlights

- Fixed the task detail drawer regression in `frontend/src/App.vue` so task detail loading again receives the correct cluster state and opens reliably from the split task workspace.
- Restored frontend button and tag presentation to match the `main` visual baseline by reactivating the shared Element Plus override selectors after the component extraction work.
- Updated Playwright E2E coverage for split-view navigation, empty states, alerts, task detail, and retry-upload flows so the suite exercises the current workspace layout instead of the old single-page assumptions.
- Removed the flaky retry-upload E2E route-session reuse pattern by validating the second phase of the scenario in a fresh browser page.

## Upgrade Notes

- No database migration is required for `v0.4.1`.
- No backend API contract changes are introduced in this release.
- This release is safe to deploy as a frontend-focused patch on top of `v0.4.0`.

## Operator Impact

- The task console is visually consistent again, especially for action buttons, status tags, and drawer controls.
- Split-view workflows for tasks, workers, sources, and alerts now match the regression tests and are less likely to drift again.
- Retry-upload and task detail interactions are more reliable in automated and manual verification.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.4.1.zh-CN.md
