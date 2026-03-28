# Binlog Server v0.2.0

Binlog Server `v0.2.0` adds internationalization (i18n) support to the frontend, enabling operators to switch between Simplified Chinese and English. This release also fixes Element Plus component locale binding and locale-aware timestamp formatting.

## Highlights

- Added full i18n support using `vue-i18n@9` with Composition API.
- Introduced locale files covering all 228 user-facing strings in both `zh-CN` and `en`.
- Language switcher added to the Settings dialog, with preference persisted to `localStorage`.
- Element Plus components (pagination, date pickers, etc.) now follow the selected locale reactively via `el-config-provider`.
- Timestamp formatting (`formatTs`) now respects the active app locale.
- Added language navigation badges to `README.md` and `README_ZH.md`.

## Upgrade Notes

- No schema migration is required for `v0.2.0`.
- No breaking backend API contract change is introduced in this release.
- If you package frontend static assets separately, ensure the refreshed frontend bundle is deployed together with the `v0.2.0` binary.

## Operator Impact

- Operators can switch the console UI language between Simplified Chinese and English from Settings.
- Language preference is remembered across browser sessions.
- All UI text, element labels, and status messages are now fully translated.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.2.0.zh-CN.md
