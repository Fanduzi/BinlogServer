---
name: check-three-level-doc
description: Run automated loop checks for three-level documentation compliance after code changes. Use when verifying changed files for required L3 file-header declarations and required L2 module README.md synchronization before commit or CI merge.
---

# Check Three-level Doc

## Overview

Validate three-level documentation loop requirements against current git changes.

## Command

Run checker from the skill folder or by absolute path:

```bash
scripts/check_three_level_doc.sh
scripts/check_three_level_doc.sh --staged
```

## What It Checks

1. L3 file header presence for changed source files:
- `input`
- `output`
- `pos`
- `note`
- For Go files: package comment in `Package [package name] ...` form.
- For Go files: package comment must appear before `input/output/pos/note`.

2. L2 module doc synchronization:
- For each module with changed source files, `<module-dir>/README.md` must exist.
- The same module `README.md` must be included in current change set.

3. L1 reminder:
- If module `README.md` changed, remind to confirm whether root `README.md` needs update.

## Exit Behavior

- Exit `0`: all hard checks passed.
- Exit non-zero: at least one hard check failed.

## Recommended Integration

- pre-commit: `scripts/check_three_level_doc.sh --staged`
- CI: `scripts/check_three_level_doc.sh`
