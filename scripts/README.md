# scripts Module

仓库级辅助脚本入口，聚合构建与 E2E 流程脚本。

## Files

| File | Responsibility |
|------|---------------|
| build-ui.sh | 构建 frontend 并同步到 internal/ui/static；先跑 `npm run test:locales` 编译 Console 文案，再给本次产物的 JS 按字节拼上 L3 头，然后跑 `check-ui-bundle.sh`（ESM 语法和 PITR 标记）；本地没有 vite 时先 `npm ci` |
| check-ui-bundle.sh | 把 `internal/ui/static/index.html` 引用的 JS，以及这些文件 import 的 chunk，当 ES module 做 `node --check --input-type=module`；并要求 `frontend/src` 里的 PITR 标记（`task-pitr`、停止/开始、生成、复制、下载、`stop_datetime`、`start_datetime`、`停止时间`）出现在这张 bundle 图里。缺标记时退出非 0。`.js` 路径上的 `node --check` 会按 CommonJS 解析，抓不到 Chrome 拒绝的 import |
| check-linux-compat.sh | 检查 Linux 二进制是否为静态链接且无动态 libc 依赖，防止发布产物绑定构建机 glibc |
| check-linux-release-archive.sh | 解包 Linux release tar.gz，校验服务端、可执行 migrate、双向 migration SQL，并复用 glibc 兼容性检查 |
| check-landing-assets.sh | 校验落地页 HTML 引用的图片资源真实存在、非空且为有效 PNG 格式，防止部署回退为 HTML 造成图裂 |
| release-assets.sh | 构建含服务端、migrate、migration SQL、`docs/guide` 与 checksums 的多平台 release 归档；作为本地/手工发版兜底入口。`docs/guide` 与 GoReleaser 归档一致 |
| verify-phase-acceptance.sh | 统一执行阶段验收命令（test/race/vet/e2e-quick）并输出耗时摘要 |
| failover-dogfood.sh | 一个 meta MySQL 上的双 worker 租约接管复跑；控制面走 verify-binlog-server helper |
| e2e/ | E2E 套件与场景脚本 |

## Exports

- `make build [VERSION=v0.1.0]`（默认 `CGO_ENABLED=0`）
- `make build-linux [VERSION=v0.1.0]`（Linux 发布二进制默认 `CGO_ENABLED=0`，避免绑定构建机 glibc）
- `make check-linux-compat [VERSION=v0.1.0]`（只编 Linux 二进制做静态链接检查，不重建 frontend，避免弄脏 git 树）
- `make check-linux-release-archive VERSION=v0.1.0`
- `make check-landing-assets`
- `make ui-build`
- `./scripts/check-ui-bundle.sh`（ESM 语法，以及 frontend PITR 标记必须出现在 `index.html` 加载的 bundle 图里；CI 与 release workflow 在发布前跑这条）
- `make release-assets VERSION=v0.1.0`
- `make e2e-quick`
- `make e2e-full`
- `./scripts/verify-phase-acceptance.sh`
- `BINLOG_SERVER_BIN=... ./scripts/failover-dogfood.sh run happy|backoff|no-steal|no-stomp|failed|pause|cleanup`
- `failover-dogfood.sh` 的 `RETRY_BACKOFF` 接管不要求 `TASK_LEASE_TAKEOVER`（该事件只在先前状态为 `RUNNING` 或 `LEASE_DEGRADED` 时写入）；通过条件是 owner 变为 `worker-b`、epoch 增大，且没有 `STOPPED`。
- `perf` 计时把场景标准输出收进命令替换；源库插入循环的标准输出另开，避免把采样管道一直占住。
- Console 截图通过本机 Chrome DevTools 打开 `/ui/`，并允许 `127.0.0.1` 的调试来源。

## Dependencies

- Upstream: Makefile / 开发者本地命令 / GitHub Actions release workflow。
- Downstream: frontend 构建工具链、Docker、Go 服务进程、GitHub Release 资产发布。
- `failover-dogfood.sh` 额外依赖本机 `mysqld`、`mysql`、已发布的 `binlog-server` 与旁边的 `migrate`，以及 `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh`。

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
