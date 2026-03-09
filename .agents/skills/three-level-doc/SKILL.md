---
name: three-level-doc
description: "Define and enforce a strict three-level documentation protocol for every code change. Use when developing, reviewing, or refactoring any project that requires L1 root README architecture map, L2 module README files, and mandatory L3 file-header input/output/pos declarations with post-change loop checks."
---

# Three-level Doc

## Overview

Enforce a fixed three-level documentation contract and treat it as part of code correctness.

## Fixed Structure (Mandatory)

### L1 (Global Map)

File path is fixed:
- `README.md` (root)

Rules:
- Root `README.md` must contain and maintain:
  - `## Architecture`
  - `### Modules` (module table + links to L2)
- Any global architecture/collaboration change must update this section in the same change set.

### L2 (Module Member + Interface Doc)

File path is fixed per module directory:
- `<module-dir>/README.md`

Rules:
- Every module directory must contain exactly one module doc named `README.md`.
- Module doc must contain:
  - Member list (key files/components + responsibility)
  - Interface list (externally exposed APIs/contracts/commands/events)
  - Dependency summary (upstream/downstream)
- Any member/interface/dependency change must update this file in the same change set.

### L3 (File Header Declaration)

Header position is fixed:
- At the beginning of each source file.

Header content is fixed:
- first line: `Package [package name] ...` (Go files)
- then 4 lines:
- `input`
- `output`
- `pos`
- mandatory note line

Required header format:

```text
// Package <package_name> ...
// input: <external dependencies consumed by this file>
// output: <externally visible outputs provided by this file>
// pos: <local architectural role of this file>
// note: if this file changes, update this header and module README.md.
```

Rules:
- Every source file must have this header block.
- If file behavior/dependencies/role changes, update header in the same change set.
- Every Go source file should include package comment in this form:
  - `// Package [package name] ...`

## Mandatory Post-change Loop Check

Run this loop before completion for every code change:

1. L3 loop:
- Re-check changed files' `input/output/pos` against real code.
- Update file header immediately when mismatch is found.
- For Go files, re-check package comment format:
  - `// Package [package name] ...`

2. L2 loop:
- Re-check impacted modules' member/interface/dependency lists.
- Update `<module-dir>/README.md` immediately when mismatch is found.

3. L1 loop:
- Re-check root `README.md` architecture/modules section when change affects global structure.
- Update root `README.md` when needed.

Completion gate:
- Do not mark task complete until all required L1/L2/L3 updates are done.


## Enforcement Rules

- Missing L1/L2/L3 documentation is a defect.
- Outdated L1/L2/L3 documentation is a defect.
- Code and documentation updates must be co-committed.

## References

- See `references/templates.md` for fixed templates.
- For automated loop checks, use `$check-three-level-doc`.
