# scripts Module

仓库级辅助脚本入口，聚合构建与 E2E 流程脚本。

## Files

| File | Responsibility |
|------|---------------|
| build-ui.sh | 构建 frontend 并同步到 internal/ui/static |
| release-assets.sh | 构建带内嵌 UI 的多平台 release 二进制、压缩包与 checksums |
| verify-phase-acceptance.sh | 统一执行阶段验收命令（test/race/vet/e2e-quick）并输出耗时摘要 |
| e2e/ | E2E 套件与场景脚本 |

## Exports

- `make ui-build`
- `make release-assets VERSION=v0.1.0`
- `make e2e-quick`
- `make e2e-full`
- `./scripts/verify-phase-acceptance.sh`

## Dependencies

- Upstream: Makefile / 开发者本地命令。
- Downstream: frontend 构建工具链、Docker、Go 服务进程。

## Update Rule

- 脚本入口、执行依赖或调用约定变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
