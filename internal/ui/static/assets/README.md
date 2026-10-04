# internal/ui/static/assets Module

前端构建生成的静态资源子目录。

## Files

| File | Responsibility |
|------|---------------|
| `index-Dj2U7sKW.js` | 当前前端 entry bundle。任务详情回放区按 `GET /api/tasks/{id}/replay` 显示每个源序号一条磁盘路径，并一次复制 `mysqlbinlog` 或 `mariadb-binlog` 命令。遗留目录任务详情有「认领」，提交 `POST /api/tasks/{id}/adopt`，状态保持 STOPPED，不回显密码，也不启动复制。任务文件表显示磁盘文件名和 `file_path`（open 分段含 `.open.e<epoch>`）。Flavor 为 mysql/mariadb 下拉，hint 用 vue-i18n 字面量转义 `@@server_uuid`。刷新只编排一次任务/集群/workers，列表租约风险用任务抄本；`single_process` 时总览与 Worker 页写明本进程拉取；由 `make ui-build` 生成并保留 L3 声明。import 列表里不能有多余的 `}`，否则 Console 无法挂载 |
| * | 由前端构建工具生成的其他资源文件；历史与 vendor 资源保留 |

## Exports

- 作为 `/ui/` 资源子路径被浏览器按需加载。
- 当前 `index-Dj2U7sKW.js` 与 `internal/ui/static/index.html` 成对发布；回放命令、认领、任务文件路径、Flavor 下拉、任务观测或单机 `single_process` 文案变更后由 `make ui-build` 更新。仅删除被新 index 直接替代的旧 entry。
- `make ui-build` 用字节拼接写入 L3 头，不改 minified 正文；随后 `scripts/check-ui-bundle.sh` 用 `node --check --input-type=module` 检查 `index.html` 引用的 JS。语法错误则构建失败。

## Dependencies

- Upstream: frontend 构建输出。
- Downstream: 浏览器资源请求。

## Update Rule

- 资源组织方式变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
