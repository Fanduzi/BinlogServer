# Open Source Entry Docs Refresh

**Date:** 2026-03-09
**Scope:** repository entry-layer documentation only
**Out of scope:** `docs/guide/*`, business code, CI behavior, config semantics

## Goal

Make the repository landing experience usable for a first-time open source visitor:

- understand what the project does
- get the service running quickly
- verify health
- create a first task
- know where production risks and deeper docs live

## Changes

### README reorganization

Reworked the root `README.md` into a shorter entry document:

- project framing (`what it solves`, `who it is for`, `when not to use it`)
- concise product summary
- quick start with executable commands
- expected result after quick start
- first-task flow
- minimal production notes
- common pitfalls
- upgrade/release entry
- compact repository map

### Linking strategy

The README now keeps only summary-level guidance and links out to:

- `docs/guide/README.md`
- `SECURITY.md`
- `CHANGELOG.md`
- command/module READMEs

This keeps the repository root useful without duplicating guide content.

## Why This Does Not Conflict With `docs/guide/*`

The root README now acts as an entry layer, not an operations manual.
Detailed procedures remain in existing guide and module docs.
No guide content was rewritten or moved.

## Verification

- `git diff --check`
- YAML/config semantics unchanged by inspection
- Markdown structure self-check via section and link review
