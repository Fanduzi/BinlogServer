# Three-level Doc Templates (Fixed)

## L1 Template (fixed path: root `README.md`)

```markdown
# Project Name

... existing readme content ...

## Architecture

Brief architecture description.

### Modules

| Module | Description | Doc |
|--------|-------------|-----|
| api | HTTP handlers | [README](internal/api/README.md) |
| store | Data access | [README](internal/store/README.md) |
```

## L2 Template (fixed path: `<module-dir>/README.md`)

```markdown
# <Module Name> Module

<module purpose summary>

## Files

| File | Responsibility |
|------|---------------|
| server.go | Gin server setup |
| handlers.go | Request handlers |

## Exports

- `NewServer() *Server`
- `RegisterRoutes(r *gin.Engine)`

## Dependencies
- Upstream: ...
- Downstream: ...

## Update Rule
- If members/interfaces/dependencies change, update this file in same change.
```

## L3 Template (fixed location: beginning of every source file)

```text
// Package <package_name> ...
// input: <external dependencies consumed by this file>
// output: <externally visible outputs provided by this file>
// pos: <local architectural role of this file>
// note: if this file changes, update this header and module README.md.
```

## Mandatory Loop Checklist

```markdown
- [ ] L3 updated for every changed source file (input/output/pos accurate)
- [ ] L2 updated for every impacted module (`<module-dir>/README.md`)
- [ ] L1 updated when repository architecture/modules changed (root `README.md`)
- [ ] Code + docs committed together
```
