---
name: binlogserver-release
description: Release Binlog Server. One prep PR (bilingual notes, the CHANGELOG Unreleased section renamed to the version, VERSION and install pins, landing version strings), make release-check, fast-forward land, annotated "Release vX.Y.Z" tag by Fan(), watch release.yml, verify assets, the Cloudflare Pages deploy and the live landing page. Use when asked to release, tag or ship Binlog Server (发布, 发版, 上线, 打tag).
---

# Binlog Server Release

Publish Binlog Server with goreleaser from a tag. The steps follow the
maintainer's original `go-release` workflow (version, merge, notes, surface
audit, commit, tag, push, clean), adapted to this repo: a reviewed prep PR,
`make release-check`, fast-forward-only landing, and checks after publishing.
The human checklist is [docs/RELEASING.md](../../../docs/RELEASING.md).

Passing tests does not make a release. Verify the whole chain:
`prep commit -> tag -> release.yml -> archives -> GitHub Release assets -> install docs -> landing page`.

## Hard rules

- Every commit has author **and** committer `Fan() <18501341937@163.com>`.
  Every tag has tagger `Fan() <18501341937@163.com>`.
- No AI-tool attribution or names in commits, branches, PR titles or bodies,
  tag messages or release notes. No `Co-authored-by` or `Generated-by` trailers.
- Never force-push. Never move, delete or recreate a pushed tag or a published
  GitHub Release. A broken release is fixed by the next patch version.
- `main` moves only by fast-forward:
  `gh api -X PATCH repos/Fanduzi/BinlogServer/git/refs/heads/main -f sha=<sha> -F force=false`.
  No merge button, no `git push origin main`, no merge commits.
- Do not land while another agent is still pushing to the branch.
- Push only the one tag: `git push origin vX.Y.Z`. Never `--tags`.
- Never commit delivery evidence (QA transcripts, `docs/agents/delivery-*.md`,
  run logs, screenshots taken as proof). Evidence goes in the PR or QA thread.

## Cadence

- At most one release per day.
- Release only when `main` has at least one user-visible change since the
  last tag (`CHANGELOG.md` `## [Unreleased]` is not empty).
- A P0/P1 fix may ship as a hotfix release right away, even on the same day.
- Never release once per PR. A release is one batch: one tip QA, one prep PR,
  one tag.

## Step 0: Pre-flight

```bash
git fetch origin --tags
git switch main && git pull --ff-only origin main
git status --porcelain              # must be empty
git rev-parse HEAD origin/main      # must be equal
LAST_TAG=$(git describe --tags --abbrev=0)
git log -1 --format=%cs "$LAST_TAG" # not today, unless this is a P0/P1 hotfix
git log --oneline "${LAST_TAG}..origin/main"   # the batch; must have a user-visible change
git tag --points-at HEAD            # must be empty
gh release view vX.Y.Z -R Fanduzi/BinlogServer   # must be "release not found"
git ls-remote --tags origin "vX.Y.Z*"             # must be empty
```

- **Tip QA PASS on the exact SHA.** The QA verdict must name the current
  `origin/main` SHA and say it is OK to tag. A PASS on an older SHA does not
  count; re-run tip QA if `main` moved.
- Push CI is green on that SHA: `gh run list -R Fanduzi/BinlogServer --commit <sha>`.
- Identity and attribution scan over the batch; both commands must print nothing:

```bash
git log --format='%H %an <%ae> | %cn <%ce>' "${LAST_TAG}..origin/main" \
  | grep -v 'Fan() <18501341937@163.com> | Fan() <18501341937@163.com>'
git log --format='%B' "${LAST_TAG}..origin/main" \
  | grep -inE 'co-authored-by|generated-by|generated with|🤖'
```

## Step 1: Choose the version

Semver over the batch:

| Commit pattern in the batch | Bump |
|---|---|
| `feat!:`, `fix!:`, or a breaking change called out in an ADR | MINOR while < 1.0, MAJOR after |
| `feat:` | MINOR |
| `fix:` and everything else | PATCH |

While the project is 0.x, most batches are PATCH and a product turn is MINOR.
Put the reasoning and the chosen `vX.Y.Z` in the prep PR body.

## Step 2: Run the gates on the candidate SHA

On a clean checkout of the candidate SHA. A skip counts as a failure.

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

Also run `make e2e-full` when the batch touches replication, cluster or
failover, retention, or upload. The PR CI scenario matrix in `e2e.yml` must be
green on the candidate SHA. If any gate fails, report the failing command and
its output, and stop.

## Step 3: The prep PR (one PR, one commit)

Branch `release/vX.Y.Z`. One commit, `docs: prepare Binlog Server vX.Y.Z`.
Release surfaces only; no code.

### 3a. Release notes (EN and zh)

The paths are fixed by `release.yml` and `scripts/check-release-prep.sh`.
Read the H1 patterns in the script before writing.

- `docs/releases/release-notes-vX.Y.Z.md`: English. This file **is** the
  GitHub Release body.
- `docs/releases/vX.Y.Z.zh-CN.md`: Chinese.

No third `docs/releases/vX.Y.Z.md` file. 20 to 40 lines each. No QA
transcript, no full PR list, no checklist tallies.

English template:

```markdown
# Binlog Server vX.Y.Z

Binlog Server `vX.Y.Z` is a <patch|minor> release focused on <one sentence>.

## Highlights

- <user- or operator-visible change, one line each, 3 to 6 bullets>

## Upgrade Notes

- Schema: <"No database migration is required." | "Run `./migrate up` (schema N to M) before restarting.">
- Config: <"No config key changes." | key, default, whether old config still works>
- API: <"No backend API contract changes." | what changed>
- Observability: <metrics, labels or alerts operators must notice, or "None.">

## Operator Impact

- <what an operator notices after upgrading, 1 to 3 bullets>

## Known Issues

- <#NNN, one line each, or "None.">

## Chinese Release Notes

https://github.com/Fanduzi/BinlogServer/blob/vX.Y.Z/docs/releases/vX.Y.Z.zh-CN.md
```

Chinese template:

```markdown
# Binlog Server vX.Y.Z 中文发布说明

> 对应版本：`vX.Y.Z`

`vX.Y.Z` 是一次<补丁/功能>版本：<一句话>。

## 核心变化

- <每条一行，3 到 6 条>

## 升级影响

- Schema：<无需数据库迁移 | 重启前先执行 `./migrate up`（schema N 到 M）>
- 配置：<无配置项变化 | 变化的 key、默认值、旧配置是否可用>
- API：<无后端 API 契约变更 | 变化>
- 可观测性：<指标、标签、告警变化，或“无”>

## 运维收益

- <1 到 3 条>

## 已知问题

- <#NNN 一行一条，或“无”>
```

### 3b. CHANGELOG.md

Keep a Changelog style. PRs append their user-visible changes to
`## [Unreleased]` as they land. The prep commit:

1. Renames `## [Unreleased]` to `## [vX.Y.Z] - YYYY-MM-DD`.
2. Adds a fresh, empty `## [Unreleased]` above it.
3. Trims the entry to at most 15 lines, user- and operator-facing only
   (`### Added`, `### Changed`, `### Fixed`). Schema, config, sqlc and
   observability changes must be called out. Link the release notes rather
   than repeating them.

Do not rewrite older entries in a release PR.

### 3c. Version pins

`TestVersionPinsMatchVERSIONFile` requires these to agree with `VERSION`.
Change them together:

- `VERSION`: `X.Y.Z`
- `README.md`, `README_ZH.md`, `docs/guide/admin/deployment.md`: `VER=X.Y.Z`
- `config.example.yaml`, `config.production.example.yaml`: `vX.Y.Z` header

README rules:
- Change the pins only. No per-version upgrade bullet, no "What's new"
  section, no release history in either README.
- Per-version upgrade notes go in `docs/guide/admin/upgrade.md` and
  `docs/guide/admin/upgrade.zh-CN.md`, newest first.
- The README size and section guard in CI must stay green.

### 3d. Landing page (`docs/landing/index.html`)

The landing page is a product page, not a release log and not a README dump.
It names only the current version. Two strings are pinned by the test:

- `vX.Y.Z — latest release` (hero badge)
- `Get Started with vX.Y.Z` (closing call to action)

Replace the old version everywhere on the page: the static HTML, the EN i18n
strings and the zh i18n strings. That includes the Quick Start button and the
Step 1 title, which also carry the version.

```bash
OLD=${LAST_TAG#v}   # previous release, e.g. 0.5.83
sed -i "s/v${OLD}/vX.Y.Z/g" docs/landing/index.html   # the page names only the previous version
rg -o 'v[0-9]+\.[0-9]+\.[0-9]+' docs/landing/index.html | sort -u   # only vX.Y.Z
```

`TestLandingPageHasNoReleaseHistory` fails on a release timeline or
"Recent Releases" section, a hardcoded checksum, any version other than
`VERSION`, or a page over 96 KiB. Do not add release cards, dates, issue
numbers, QA tallies or known-issue lists. Change product copy only when the
batch changed what the product does, using facts from the README and docs.

### 3e. Surface audit

```bash
NEXT=X.Y.Z
SURFACE="README.md README_ZH.md docs/landing/index.html docs/guide/admin/deployment.md config.example.yaml config.production.example.yaml VERSION"
rg -n 'v?[0-9]+\.[0-9]+\.[0-9]+' $SURFACE | grep -v "$NEXT" | grep -v 'release-notes-v'
```

Older versions may remain only in history: `docs/releases/*`, older CHANGELOG
entries, the upgrade guide, ADRs and plans. They must not remain in "current
release" text, download commands or install snippets. Dependency versions
(MySQL 8.4, Go 1.x) are fine.

### 3f. The gate

```bash
go test ./internal/config/ -run 'TestVersionPinsMatchVERSIONFile|TestLandingPageHasNoReleaseHistory'
make release-check VERSION=vX.Y.Z
git diff --check origin/main...HEAD
```

`make release-check` must print
`[release-check] all release surfaces ready for vX.Y.Z`. `release.yml` runs
the same check on the tag before building.

### 3g. Open and land the prep PR

1. Push the branch and open the PR, titled with the commit subject. The body
   holds the version reasoning, the batch (PR numbers, one line each) and the
   gate results. Apply a clean body with
   `gh api -X PATCH repos/Fanduzi/BinlogServer/pulls/N -F body=@body.md`.
2. All PR checks are green on the head SHA.
3. A quick QA pass on the prep PR: content, links, landing preview.
4. Land by fast-forward only:

```bash
gh api repos/Fanduzi/BinlogServer/compare/main...release/vX.Y.Z --jq '.behind_by,.ahead_by'   # 0 and 1
PREP_SHA=$(gh api repos/Fanduzi/BinlogServer/git/refs/heads/release/vX.Y.Z --jq .object.sha)
gh api -X PATCH repos/Fanduzi/BinlogServer/git/refs/heads/main -f sha=$PREP_SHA -F force=false
gh api -X DELETE repos/Fanduzi/BinlogServer/git/refs/heads/release/vX.Y.Z
```

5. Push CI on `main` at `$PREP_SHA` is green.

## Step 4: Tag

Annotated, on the prep commit:

```bash
git fetch origin && git switch main && git pull --ff-only origin main
test "$(git rev-parse HEAD)" = "$PREP_SHA"
make release-check VERSION=vX.Y.Z REF=HEAD
GIT_COMMITTER_NAME='Fan()' GIT_COMMITTER_EMAIL='18501341937@163.com' \
  git -c user.name='Fan()' -c user.email='18501341937@163.com' \
  tag -a vX.Y.Z -m "Release vX.Y.Z" "$PREP_SHA"
git cat-file -p vX.Y.Z | head -5   # tagger Fan() <18501341937@163.com>; message "Release vX.Y.Z"
git push origin vX.Y.Z             # starts the release
```

## Step 5: Watch release.yml

```bash
gh run list -R Fanduzi/BinlogServer --workflow release.yml --limit 3
gh run watch <run-id> -R Fanduzi/BinlogServer --exit-status
gh run view <run-id> -R Fanduzi/BinlogServer --json status,conclusion,jobs,url
```

Every job must succeed: `ci` (e2e.yml), the release-prep check, the UI and
landing checks, unit tests, goreleaser, Linux compatibility, the archive check,
publish, and the published-archive check. A green build is an Actions artifact,
not a Release asset. On failure, find the first failed job and diagnose it;
fix on `main` and ship the next patch. Do not touch the tag.

## Step 6: Verify the release

```bash
gh release view vX.Y.Z -R Fanduzi/BinlogServer --json name,tagName,isDraft,isPrerelease,assets,url
```

- Not a draft, not a prerelease. The body is the English notes and ends with
  the Chinese link.
- Exactly 5 assets: `binlog-server_X.Y.Z_{darwin,linux}_{amd64,arm64}.tar.gz`
  and `checksums.txt`.

```bash
d=$(mktemp -d) && cd "$d"
gh release download vX.Y.Z -R Fanduzi/BinlogServer -p 'binlog-server_X.Y.Z_linux_amd64.tar.gz' -p checksums.txt
sha256sum -c --ignore-missing checksums.txt   # ": OK"
tar -xzf binlog-server_X.Y.Z_linux_amd64.tar.gz
./binlog-server version                      # reports vX.Y.Z and commit $PREP_SHA
```

## Step 7: Verify the Cloudflare Pages deploy and the live page

Cloudflare Pages deploys `docs/landing/` from `main` as a GitHub check, not a
workflow. Confirm it succeeded on `$PREP_SHA`, then read the live page:

```bash
gh api repos/Fanduzi/BinlogServer/commits/$PREP_SHA/check-runs \
  --jq '.check_runs[] | select(.name | test("Cloudflare Pages")) | "\(.name) \(.conclusion)"'   # success
curl -fsSL https://binlogserver.pages.dev | grep -c 'vX.Y.Z — latest release'                  # >= 1
curl -fsSL https://binlogserver.pages.dev | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | sort -u        # only vX.Y.Z
```

Open it once in a browser: no broken images, no blank gaps.

## Step 8: After the release

- `vX.Y.Z^{}` equals `$PREP_SHA`. The previous tag is unchanged.
- Scan the tag message and release body for AI-tool attribution.
- No follow-up "point docs at vX.Y.Z" PR should be needed. If one is, the
  prep was incomplete; fix this skill or `check-release-prep.sh`.
- `git worktree prune`; delete the release branch if it is still there.
- No README or landing accretion: nothing is added to either outside the next
  prep PR.
- Report one status: `released and verified`, or `blocked with evidence`
  (the first unmet step, the exact SHA, the command output, the safe next step).

## Checklist

Before the tag:
- [ ] No release yet today (or a P0/P1 hotfix); a user-visible change since the last tag
- [ ] Tip QA PASS on the exact `main` SHA; push CI green
- [ ] Clean tree; behind 0; the tag and release do not exist
- [ ] Batch commits by Fan(), no AI-tool attribution
- [ ] Step 2 gates pass
- [ ] One prep commit: EN and zh notes, `Unreleased` renamed (≤ 15 lines), VERSION and pins, landing version strings
- [ ] README has pins only; no evidence files committed
- [ ] `make release-check VERSION=vX.Y.Z` passes
- [ ] Prep PR green, checked, fast-forwarded, branch deleted

After the tag:
- [ ] Annotated `Release vX.Y.Z` tag by Fan() on the prep commit, pushed alone
- [ ] release.yml fully green
- [ ] 4 archives and `checksums.txt`; checksum OK; `binlog-server version` reports vX.Y.Z
- [ ] Cloudflare Pages check succeeded; binlogserver.pages.dev shows only vX.Y.Z

## Recovery

| Problem | What to do |
|---|---|
| `make release-check` fails | Fix the named surface in the prep PR; re-run |
| Prep branch is behind `main` | Rebase the prep branch (not public history yet); re-run the gates |
| release.yml fails before publishing | Diagnose the first failed job; fix on `main`; release vX.Y.(Z+1). Do not move the tag |
| Release published with wrong text | Edit the release body only; otherwise ship the next patch |
| Live landing page shows an old version | Check the Cloudflare Pages check; fix through a PR; verify again |
| The tag already exists | Stop. Do not delete it. Choose the next version |
