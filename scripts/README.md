# scripts Module

仓库级辅助脚本入口，聚合构建与 E2E 流程脚本。

## Files

| File | Responsibility |
|------|---------------|
| build-ui.sh | 构建 frontend 并同步到 internal/ui/static；本地没有 vite 时先 `npm ci` |
| check-linux-compat.sh | 检查 Linux 二进制是否为静态链接且无动态 libc 依赖，防止发布产物绑定构建机 glibc |
| check-linux-release-archive.sh | 解包 Linux release tar.gz，校验服务端、可执行 migrate、双向 migration SQL，并复用 glibc 兼容性检查 |
| release-assets.sh | 构建含服务端、migrate、migration SQL、文档与 checksums 的多平台 release 归档；作为本地/手工发版兜底入口 |
| verify-phase-acceptance.sh | 统一执行阶段验收命令（test/race/vet/e2e-quick）并输出耗时摘要 |
| e2e/ | E2E 套件与场景脚本 |

## Exports

- `make build [VERSION=v0.1.0]`（默认 `CGO_ENABLED=0`）
- `make build-linux [VERSION=v0.1.0]`（Linux 发布二进制默认 `CGO_ENABLED=0`，避免绑定构建机 glibc）
- `make check-linux-compat [VERSION=v0.1.0]`（只编 Linux 二进制做静态链接检查，不重建 frontend，避免弄脏 git 树）
- `make check-linux-release-archive VERSION=v0.1.0`
- `make ui-build`
- `make release-assets VERSION=v0.1.0`
- `make e2e-quick`
- `make e2e-full`
- `./scripts/verify-phase-acceptance.sh`

## Dependencies

- Upstream: Makefile / 开发者本地命令 / GitHub Actions release workflow。
- Downstream: frontend 构建工具链、Docker、Go 服务进程、GitHub Release 资产发布。

## Release Notes

- tag 版本发布默认由 `.github/workflows/release.yml` + `.goreleaser.yml` 自动构建并发布 GitHub Release 资产。
- release workflow 会先执行一次 `goreleaser release --skip=publish,announce` 生成真实 release archive，再运行 `make check-linux-release-archive` 校验必需资产与 Linux 二进制，最后才正式发布。
- `release-assets.sh` 保留为本地验证和手工补发资产时的兜底入口，并与 GoReleaser 共享相同归档内容契约。
- Linux 分发二进制统一以 `CGO_ENABLED=0` 构建，兼容老环境中的 `glibc 2.17` 基线。
- 每个 tag 发布前需要先提交英文主 release note：`docs/releases/release-notes-vX.Y.Z.md`。

## Update Rule

- 脚本入口、执行依赖或调用约定变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
