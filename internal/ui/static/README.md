# internal/ui/static Module

Console（`console/`，React）构建产物目录，由后端以静态资源方式对外提供 `/ui/`。

## Files

| File | Responsibility |
|------|---------------|
| index.html | Console 入口页面，引用当前 Vite hashed bundles（base `/ui/`） |
| assets/ | 构建资源：entry、vendor chunk、按路由拆分的页面 chunk 和样式 |

## Exports

- 被 `internal/ui` 的 `Handler()` 挂载并通过 `/ui/` 提供访问。
- 由 `make ui-build`（`scripts/build-ui.sh`）整目录替换（保留 README）。`scripts/check-ui-bundle.sh` 沿 `index.html` 的静态和动态 import 走完整个 bundle 图，要求每个 chunk 是合法 ESM，且 `console/src` 里的 PITR、可恢复窗口、换主、`task-storage-alert`、`task-replay-damaged` 标记都在图里。

## Dependencies

- Upstream: `console` 构建流程。
- Downstream: 浏览器静态资源加载。

## Update Rule

- 构建产物布局或挂载约定变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
