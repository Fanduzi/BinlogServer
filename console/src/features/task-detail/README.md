# console/src/features/task-detail

任务详情抽屉：可恢复窗口、回放命令、定点恢复和分段下载。

## Files

| File | Responsibility |
|------|----------------|
| `task-detail-sheet.tsx` | 任务详情界面。当前状态旁边显示期望状态 `desired_run`（`RUN` 或 `STOP`），`data-testid` 是 `task-desired-run` |
| `use-task-detail.ts` | 详情数据加载 |
| `detail-format.ts` | 回放命令和位点文案。有客户端时命令以 `set -o pipefail` 开头；`mariadb-binlog` 带 `--skip-gtid-strict-mode`；末尾注释要求在同一 shell 里管道并核对行数或 GTID。标准输出仍是 SQL |
| `detail-format.test.ts` | 上述格式化的单元测试 |

## Interfaces

- `formatReplayCommand(replay)`：从 `replay.client` 和 `replay.paths` 生成可复制的多文件回放命令。
- `formatClientCommand(client, tokens)`：与 `internal/tasks` 的 `formatClientCommand` 同一形状。

## Dependencies

- Upstream: `GET /api/tasks/{id}/replay` 的 `paths` 和 `client`；定点恢复用响应里的 `command`。
- Downstream: 任务详情抽屉的复制按钮。

## Update Rule

- 回放命令形状变化时，更新本文件，并保持与 Go 生成的 `command` 一致。
