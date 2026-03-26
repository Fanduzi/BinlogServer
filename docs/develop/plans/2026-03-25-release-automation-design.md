# Release Automation Design

## Scope

This design covers two goals:

1. Repair the existing `v0.1.1` GitHub Release so it matches the `v0.1.0` style: English primary release body, Chinese note linked separately, and uploaded binary assets.
2. Add CI-based tag-triggered release automation for future versions using GoReleaser and GitHub Actions.

## Current Problems

- The existing `v0.1.1` GitHub Release body is Chinese, while the repository convention established by `v0.1.0` is an English primary release body plus a separate Chinese note link.
- The existing `v0.1.1` GitHub Release has no uploaded binary assets, even though the repository already has a local packaging script at `scripts/release-assets.sh`.
- The repository does not yet have a dedicated release workflow under `.github/workflows/` or a `.goreleaser` configuration.
- The current process is easy to forget because release note editing and asset upload are still manual.

## Goals

- Keep GitHub Release pages English-first.
- Keep Chinese release notes in `docs/releases/` as repository-hosted supplementary material.
- Ensure every tagged release automatically publishes multi-platform binaries and `checksums.txt`.
- Reuse the existing embedded UI build flow so release binaries always contain current static assets.
- Keep a local/manual fallback path for maintainers.

## Non-Goals

- Auto-generating release notes from commits or PRs in this change.
- Changing the release artifact matrix beyond the current supported targets.
- Removing the existing `scripts/release-assets.sh` script immediately.

## Options Considered

### Option A — Recommended
Use GoReleaser + GitHub Actions for tag-triggered publishing, while retaining `scripts/release-assets.sh` as a local/manual fallback.

Pros:
- Solves the missing-assets problem for future tags.
- Aligns with a standard Go release toolchain.
- Keeps the existing script available for local packaging and debugging.
- Minimizes migration risk.

Cons:
- Release notes still need a per-version English file committed before tagging.
- Some release knowledge remains split between GoReleaser config and existing scripts.

### Option B
Keep the current manual flow and improve only documentation/checklists.

Pros:
- Lowest code/config change.

Cons:
- Does not eliminate the failure mode that already happened on `v0.1.1`.
- Still depends on maintainers remembering multiple manual steps.

### Option C
Replace the existing packaging script completely with GoReleaser-only local and CI flows.

Pros:
- Single release mechanism.

Cons:
- Higher migration risk.
- Less conservative than needed for the current issue.

## Recommended Design

### Release Notes Convention

For each release tag `vX.Y.Z`:

- English GitHub Release body source: `docs/releases/release-notes-vX.Y.Z.md`
- Chinese supplementary note: `docs/releases/vX.Y.Z.zh-CN.md`

The English note should remain the primary GitHub Release body. It should end with a small appendix link pointing to the Chinese note in the repository.

### CI Release Flow

On `push` of tags matching `v*`:

1. Check out the repository with tags/history available.
2. Set up Node and Go toolchains.
3. Install frontend dependencies if needed.
4. Build and sync the embedded UI.
5. Run a minimal validation gate suitable for release CI.
6. Run GoReleaser `check`.
7. Run GoReleaser `release --clean --release-notes docs/releases/release-notes-${tag}.md`.

### Artifact Strategy

Publish the same artifact set already implied by `scripts/release-assets.sh`:

- `binlog-server_<version>_darwin_amd64.tar.gz`
- `binlog-server_<version>_darwin_arm64.tar.gz`
- `binlog-server_<version>_linux_amd64.tar.gz`
- `binlog-server_<version>_linux_arm64.tar.gz`
- `checksums.txt`

### Local Fallback Path

Keep `scripts/release-assets.sh` and `make release-assets VERSION=vX.Y.Z` available as a manual fallback and for local verification.

## Repair Plan for Existing v0.1.1

1. Create `docs/releases/release-notes-v0.1.1.md` in English.
2. Update the existing GitHub Release body for `v0.1.1` to use that English content.
3. Build `v0.1.1` release artifacts locally.
4. Upload the generated archives and `checksums.txt` to the existing `v0.1.1` GitHub Release.

## Validation

- Verify the `v0.1.1` Release body is English-first and includes the Chinese note link.
- Verify the `v0.1.1` Release lists uploaded assets.
- Verify the new workflow and GoReleaser config are syntactically valid.
- Keep changes scoped to release docs, release automation config, and any minimal supporting documentation updates.
