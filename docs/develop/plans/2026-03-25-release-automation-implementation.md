# Release Automation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Repair the existing `v0.1.1` GitHub Release and add CI-based tag-triggered release automation for future versions.

**Architecture:** Keep repository-hosted Chinese release notes as supplementary docs, add English release-note files as the GitHub Release source of truth, and use GitHub Actions plus GoReleaser to build embedded-UI multi-platform artifacts on tag push. Retain the existing local packaging script as a fallback path.

**Tech Stack:** Go, Node.js, Vite, GitHub Actions, GoReleaser, gh CLI, shell scripts.

---

### Task 1: Add English release notes for v0.1.1

**Files:**
- Create: `docs/releases/release-notes-v0.1.1.md`
- Reference: `docs/releases/v0.1.1.zh-CN.md`
- Reference: `CHANGELOG.md`

**Step 1: Draft the English release note body**
- Mirror the `v0.1.0` release style.
- Keep the body English-first.
- End with the Chinese note link.

**Step 2: Verify the note matches shipped scope**
Run: `grep -n "v0.1.1" CHANGELOG.md`
Expected: `v0.1.1` entries align with the release summary.

**Step 3: Save the release note**
- Create `docs/releases/release-notes-v0.1.1.md`.

**Step 4: Sanity-check formatting**
Run: `sed -n '1,200p' docs/releases/release-notes-v0.1.1.md`
Expected: English body with Chinese link footer.

### Task 2: Repair the live v0.1.1 GitHub Release

**Files:**
- Use: `docs/releases/release-notes-v0.1.1.md`

**Step 1: Build a release body file for `gh` usage**
Run: `cat docs/releases/release-notes-v0.1.1.md`
Expected: Complete English release body.

**Step 2: Update the GitHub Release body**
Run: `gh release edit v0.1.1 --repo Fanduzi/BinlogServer --notes-file docs/releases/release-notes-v0.1.1.md`
Expected: Release body updates successfully.

**Step 3: Verify the edited release**
Run: `gh release view v0.1.1 --repo Fanduzi/BinlogServer`
Expected: English-first body appears.

### Task 3: Build and upload v0.1.1 release assets

**Files:**
- Use: `scripts/release-assets.sh`
- Output: `dist/v0.1.1/*`

**Step 1: Build release artifacts locally**
Run: `VERSION=v0.1.1 ./scripts/release-assets.sh`
Expected: tar.gz archives plus `checksums.txt` under `dist/v0.1.1`.

**Step 2: Verify output set**
Run: `ls dist/v0.1.1`
Expected: 4 archives and `checksums.txt`.

**Step 3: Upload assets to the existing release**
Run: `gh release upload v0.1.1 dist/v0.1.1/*.tar.gz dist/v0.1.1/checksums.txt --repo Fanduzi/BinlogServer`
Expected: Upload succeeds.

**Step 4: Verify assets are visible**
Run: `gh release view v0.1.1 --repo Fanduzi/BinlogServer`
Expected: Release lists uploaded assets.

### Task 4: Add GoReleaser configuration

**Files:**
- Create: `.goreleaser.yml`
- Reference: `scripts/release-assets.sh`
- Reference: `frontend/package.json`

**Step 1: Define build matrix**
- Match current platforms: darwin/linux × amd64/arm64.
- Set ldflags for version, commit, date.

**Step 2: Define pre-build hooks**
- Install frontend dependencies if CI needs them.
- Build embedded UI before Go archives are created.

**Step 3: Define archives and checksum output**
- Match current naming convention as closely as practical.

**Step 4: Validate config**
Run: `goreleaser check`
Expected: configuration passes validation.

### Task 5: Add GitHub Actions release workflow

**Files:**
- Create: `.github/workflows/release.yml`
- Reference: `/Users/fan/GolangProjects/DeltaScope/.github/workflows/release.yml`
- Reference: `.goreleaser.yml`

**Step 1: Add tag trigger workflow**
- Trigger on `push.tags: v*`.
- Grant `contents: write`.

**Step 2: Add setup and validation steps**
- Checkout with tags.
- Setup Node and Go.
- Run `go test ./...`.
- Run GoReleaser check.

**Step 3: Add release publish step**
- Compute the matching English release note path from the tag.
- Run GoReleaser release using that path.

**Step 4: Validate workflow syntax**
Run: `gh workflow view release.yml --yaml`
Expected: workflow file is parseable.

### Task 6: Update release documentation minimally

**Files:**
- Modify: `scripts/README.md`
- Modify: `docs/releases/v0.1.1.zh-CN.md` (only if needed to remove misleading maintainer-only wording)

**Step 1: Document the new CI role**
- Keep `release-assets.sh` described as local/manual fallback.
- Mention that tagged releases are published by CI.

**Step 2: Keep Chinese note semantics clean**
- Ensure the Chinese note remains supplementary documentation, not instructions for the live GitHub Release body.

**Step 3: Sanity-check docs diff**
Run: `git diff -- scripts/README.md docs/releases/v0.1.1.zh-CN.md`
Expected: only minimal release-process wording changes.

### Task 7: Verify the release automation changes

**Files:**
- Verify all changed files

**Step 1: Run focused validation**
Run: `go test ./... && cd frontend && npm run build`
Expected: pass.

**Step 2: Run GoReleaser dry validation if available**
Run: `goreleaser check`
Expected: pass.

**Step 3: Inspect final diff**
Run: `git diff --stat`
Expected: changes limited to release docs, workflow, and GoReleaser config.
