# Binlog Server v0.4.0

Binlog Server `v0.4.0` refreshes the operations console with a more restrained visual baseline, improving hierarchy, density, and readability across the overview, task list, detail drawer, and settings dialog without changing backend contracts or core workflows.

## Highlights

- Polished the ops console hero, KPI cards, and left navigation in `frontend/src/App.vue` to establish a more professional, low-noise visual baseline with tighter spacing, calmer neutral surfaces, and clearer active/hover states.
- Refined overview, worker, and source panels so key counts and identifiers use more stable numeric typography and better grouping, making cluster status easier to scan in large-sample mock scenarios.
- Improved the task table presentation with semantic cell classes, denser row rhythm, clearer header contrast, and stronger treatment for task name / delay / ID values while preserving existing task operations and data shape.
- Upgraded the task detail drawer and settings dialog styling, including safer teleported root targeting for Element Plus overlays, so modal surfaces and action areas render consistently.

## Upgrade Notes

- No schema migration is required for `v0.4.0`.
- No backend API contract or task workflow change is introduced in this release.
- This release is focused on frontend visual polish and operator readability.

## Operator Impact

- Daily dashboard scanning is faster because overview cards, task states, and table content have stronger information hierarchy and lower visual noise.
- Task inspection surfaces are more consistent, especially in the detail drawer and settings dialog, reducing friction for routine operations.
- Existing deployment and API integrations remain unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.4.0.zh-CN.md
