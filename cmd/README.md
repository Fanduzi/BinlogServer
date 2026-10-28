# cmd Module

## Files
- `binlog-server/`: 主服务命令入口。启动失败（含 schema 版本过低）在退出前写入配置的日志文件。
- `migrate/`: 迁移命令入口。
- `release-check/`: 发版准备门禁 `release-check [-published=auto|gh|off] vX.Y.Z [git-ref]`（`scripts/check-release-prep.sh` 调它；`release.yml` 用 `-published=gh`），上一个版本是发布提交历史里有已发布 GitHub release 的最新旧 tag，它和本版本之间打过 tag 或进过 CHANGELOG 的版本也一起扫（`internal/releaseprep` 的 `scanVersions`，go test 用同一个函数）。`main.go` 只调 `internal/releaseprep.Main`，命令行、检查逻辑和测试（包括在真实仓库副本上做完整 patch/minor/major bump 的 `TestRealTreeBumpsCleanly`）都在 `internal/releaseprep`。不进发布包。

## Exports
- 提供可执行命令入口，不承载业务领域逻辑。

## Dependencies
- Upstream: 终端命令与进程管理。
- Downstream: `internal/*` 运行时模块。

## Update Rule
- 新增/调整命令入口时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
