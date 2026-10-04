# internal/ui/static/assets Module

前端构建生成的静态资源子目录。

## Files

| File | Responsibility |
|------|---------------|
| `index-fPaTka0g.js` | 当前前端 entry bundle。任务详情显示配置起点（`LATEST` / `FILE_POS file:pos` / `GTID gtid_set`）和续传 `file:pos`（有 GTID 时附上）。回放区仍下载 `task-{id}-replay.tar`。文件表每一行仍下载该磁盘文件名。遗留目录任务详情有「认领」。由 `make ui-build` 生成并保留 L3 声明。import 列表里不能有多余的 `}`，否则 Console 无法挂载 |
| * | 由前端构建工具生成的其他资源文件；历史与 vendor 资源保留 |

## Exports

- 作为 `/ui/` 资源子路径被浏览器按需加载。
- 当前 `index-fPaTka0g.js` 与 `internal/ui/static/index.html` 成对发布；起点、续传、回放集 tar、分段下载、回放命令、认领、任务文件路径、Flavor 下拉、任务观测或单机 `single_process` 文案变更后由 `make ui-build` 更新。仅删除被新 index 直接替代的旧 entry。
- `make ui-build` 用字节拼接写入 L3 头，不改 minified 正文；随后 `scripts/check-ui-bundle.sh` 用 `node --check --input-type=module` 检查 `index.html` 引用的 JS。语法错误则构建失败。

## Dependencies

- Upstream: frontend 构建输出。
- Downstream: 浏览器资源请求。

## Update Rule

- 资源组织方式变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
