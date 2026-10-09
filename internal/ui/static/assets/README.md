# internal/ui/static/assets Module

前端构建生成的静态资源子目录。

## Files

| File | Responsibility |
|------|---------------|
| `index-0P0joiPB.js` | 当前前端 entry bundle。`storage_alert` 出现时任务行和详情标出备份文件与 checkpoint 不一致，详情按 Console 语言写出损坏分段、缺失的 GTID、仍可恢复的较早分段、新任务的起点 `restart_gtid_set` 和恢复说明（旧服务端没有 `segment` 时显示英文 `message`）。`/replay` 或定点恢复结果带 `warning` 时回放区警告损坏分段及之后的分段已被排除（`task-replay-damaged`）。有 `source_chain` 时任务详情列出源服务器、当前身份、切换的 file:pos 和 GTID，继续复制的运行中任务标明仍在复制，停下的 `SOURCE_SWITCHOVER` 写出原因以及新建任务、保留备份、不要删除 `.source-chain`。文件表「服务器」列写出第几台和完整身份。`pending_dump_cleanup.connection_id` 有值时，任务详情警告源库 Binlog Dump 连接还可能开着、源库可达后会 KILL；`process_local` 为 true 时补一句只有当时那个 worker 进程看得到、重启后消失。任务详情仍显示有效本地保留天数和桶保留天数，文件表有 `location`（`local` / `bucket` / `both`），出现 `bucket` 时提示先下载再交给 mysqlbinlog。任务详情仍显示配置起点（`LATEST` / `FILE_POS file:pos` / `GTID gtid_set`）和续传 `file:pos`（有 GTID 时附上）。文件区显示可恢复窗口（最早 → 最晚，有缺口时警告「这条备份链有缺口」）。回放区仍下载 `task-{id}-replay.tar`。同一区域有 UTC 定点恢复：停止时间、停止 GTID `stop_gtid`、可选开始时间、生成并复制命令、下载该窗口。文件表每一行仍下载该磁盘文件名，并显示 `checksum`（`match` / `mismatch`）。遗留目录任务详情有「认领」。由 `make ui-build` 生成并保留 L3 声明。import 列表里不能有多余的 `}`，否则 Console 无法挂载 |
| * | 由前端构建工具生成的其他资源文件；历史与 vendor 资源保留 |

## Exports

- 作为 `/ui/` 资源子路径被浏览器按需加载。
- 当前 `index-0P0joiPB.js` 与 `internal/ui/static/index.html` 成对发布；`storage_alert`（`task-storage-alert`、`损坏的分段`）、回放排除损坏分段（`task-replay-damaged`）、起点、续传、回放集 tar、分段下载、可恢复窗口（`task-recovery-window`、`这条备份链有缺口`）、回放命令、UTC 定点恢复（`task-pitr`、`task-pitr-gtid`、`stop_datetime`、`stop_gtid`、`停止时间`、`停止 GTID`）、源服务器链（`task-source-chain`、`task-source-continued`、`task-source-stopped`、`task-source-next`）、认领、任务文件路径、`checksum` 列、`location` 列、本地/桶保留天数、Flavor 下拉、任务观测、单机 `single_process` 文案，或源库 Binlog Dump 残留警告变更后由 `make ui-build` 更新。仅删除被新 index 直接替代的旧 entry。
- `make ui-build` 用字节拼接写入 L3 头，不改 minified 正文；随后 `scripts/check-ui-bundle.sh` 用 `node --check --input-type=module` 检查 `index.html` 加载的 JS 图，并要求 `frontend/src` 的可恢复窗口标记、PITR 标记和换主标记出现在这些 bundle 里。语法错误或标记缺失则构建失败。

## Dependencies

- Upstream: frontend 构建输出。
- Downstream: 浏览器资源请求。

## Update Rule

- 资源组织方式变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
