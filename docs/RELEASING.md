# Releasing Binlog Server

The full step-by-step procedure is the release skill:
[.agents/skills/binlogserver-release/SKILL.md](../.agents/skills/binlogserver-release/SKILL.md).
This page is the short checklist. If the two disagree, fix both in the same PR.

## Rules

- At most one release per day, and only when `main` has at least one
  user-visible change since the last tag. A P0/P1 fix may ship as a hotfix
  right away. Never release once per PR.
- `main` moves only by fast-forward. No merge commits, no force-push.
- Tags are annotated, with the message `Release vX.Y.Z` and the tagger
  `Fan() <18501341937@163.com>`, on the prep commit. A pushed tag is never moved
  or deleted; a bad release is fixed by the next patch.
- No AI-tool attribution or names in commits, branches, PR titles or bodies.
- No delivery evidence files (QA transcripts, run logs) in the repository.

## 1. Gates

On a clean checkout of the `main` commit that tip QA passed:

```bash
go test ./...
go test -race ./internal/tasks ./internal/api ./internal/replication
go vet ./...
make sqlc-verify
make e2e-topology-check
make e2e-quick
make check-landing-assets
make check-linux-compat VERSION=vX.Y.Z
make release-check-test
```

Run `make e2e-full` too when the batch touches replication, failover,
retention or upload.

## 2. The prep PR

One commit, `docs: prepare Binlog Server vX.Y.Z`:

| File | Change |
|---|---|
| `docs/releases/release-notes-vX.Y.Z.md` | English notes (the GitHub Release body): Highlights, Upgrade Notes, Operator Impact, Known Issues, link to the Chinese notes. 20 to 40 lines. |
| `docs/releases/vX.Y.Z.zh-CN.md` | Chinese notes, same content. |
| `CHANGELOG.md` | Rename `## [Unreleased]` to `## [vX.Y.Z] - YYYY-MM-DD` (15 lines at most) and add a fresh empty `## [Unreleased]`. |
| `docs/guide/admin/upgrade.md`, `upgrade.zh-CN.md` | The upgrade note for vX.Y.Z, newest first. |
| `VERSION` | `X.Y.Z` |
| `README.md`, `README_ZH.md`, `docs/guide/admin/deployment.md` | `VER=X.Y.Z` pins only. |
| `config.example.yaml`, `config.production.example.yaml` | `vX.Y.Z` header. |
| `docs/landing/index.html` | Replace the previous version with vX.Y.Z (hero badge `vX.Y.Z — latest release`, `Get Started with vX.Y.Z`, and the other install strings). No release history. |

Then run `make release-check VERSION=vX.Y.Z`. It must print
`all release surfaces ready for vX.Y.Z`.

## 3. Tag

After the prep PR is fast-forwarded to `main`:

```bash
git pull --ff-only origin main
make release-check VERSION=vX.Y.Z REF=HEAD
git tag -a vX.Y.Z -m "Release vX.Y.Z"
git push origin vX.Y.Z
```

## 4. Verify

- The `release.yml` run is green.
- `gh release view vX.Y.Z` lists 4 archives and `checksums.txt`.
- One downloaded archive passes `sha256sum -c --ignore-missing checksums.txt`,
  and `binlog-server version` reports vX.Y.Z.
- The `Cloudflare Pages` check succeeded on the prep commit, and
  https://binlogserver.pages.dev shows `vX.Y.Z — latest release`.
