# Release Checklist And Changelog Convention

**Date:** 2026-03-09
**Scope:** repository release process convention only
**Out of scope:** `docs/guide/*`, CI automation, business code, deployment platform specifics

## Goal

Provide a short, repeatable release checklist for future maintainers so releases and upgrades are not handled ad hoc.

## Release Checklist

Before cutting a release:

1. Review `CHANGELOG.md` and make sure `Unreleased` reflects the actual changes.
2. Call out operator-impacting items explicitly:
   - schema / migration changes
   - config key changes or default changes
   - `sqlc` workflow or generated-code requirements
   - observability changes affecting metrics, tracing, dashboards, or alerts
3. Run the minimum release gate:

```bash
go test ./...
go test -race ./internal/tasks ./internal/api ./internal/replication
go vet ./...
make e2e-quick
```

4. If SQL or schema changed, also run:

```bash
make sqlc-verify
```

5. If security-sensitive changes landed, confirm `SECURITY.md` and any public release note wording remain accurate.
6. Set the repo-root `VERSION` file to the tag without a leading `v` (for example `0.5.76`). The same value must appear as `VER=<version>` in `README.md`, `README_ZH.md`, and `docs/guide/admin/deployment.md`, as `v<version>` in the first lines of `config.example.yaml` and `config.production.example.yaml`; `go test ./internal/config -run TestVersionPinsMatchVERSIONFile` enforces this. On the landing page (`docs/landing/index.html`) the version lives in exactly four places, all in that one file: `<html data-version="<version>">`, which the page script reads, and three no-JS fallbacks the script overwrites: the hero badge `v<version> — latest release`, the install snippet `VER=<version>`, and the CTA `Get Started with v<version>`. The EN and zh strings use `{ver}` and need no edit. A release bump on the landing page is those four strings and nothing else. The landing page is not a release log: no other version, no dates, no checksums, no new sections, headings, ids or files. `go test ./internal/config -run TestLandingPageIsAProductPage` enforces the four sites and the page's fixed shape (allowlists in `internal/config/landing_doc_test.go`); a deliberate layout change edits those lists. `docs/guide/admin/configuration.md` must name every YAML key the loader reads; `TestDocumentedConfigKeys` enforces that.

## Changelog Convention

Use `CHANGELOG.md` as the release-facing summary, not as a full internal work log.

Rules:

1. Add changes to `## [Unreleased]` as they land.
2. Only keep changes that matter to users, operators, or maintainers upgrading the system.
3. Prefer short, concrete entries over implementation detail dumps.
4. Before release, group and rewrite entries so they are readable without knowing internal ticket history.
5. When releasing, move `Unreleased` entries into a dated section and open a fresh `Unreleased` block.

## Upgrade Review Checklist

Before upgrading an environment, review whether the release contains:

- migration steps that must run before restart
- config changes that require new keys or changed defaults
- `sqlc` generation expectations for contributors or downstream maintainers
- metrics / tracing changes that may affect alerts or dashboards
- security posture changes, especially around auth defaults or exposure surface

## Release Note Expectations

If a release includes any of the following, mention them explicitly in the changelog section for that release:

- **Schema**: what changed and whether migration is required before startup
- **Config**: what key changed and whether old config still works
- **sqlc**: whether contributors need to regenerate code or update workflow assumptions
- **Observability**: whether metrics, labels, tracing, or dashboards need operator attention

## Why This Lives Here

This document is for maintainers preparing releases, so it belongs with repository maintenance plans rather than operator guides.
It complements `CHANGELOG.md`; it does not replace guide or deployment documentation.
