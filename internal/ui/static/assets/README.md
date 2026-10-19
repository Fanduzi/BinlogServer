# internal/ui/static/assets Module

Console 构建生成的静态资源子目录。

## Files

| File | Responsibility |
|------|---------------|
| `index-*.js` | Console entry bundle（React、路由、观测上下文、壳层指标卡） |
| `vendor-*.js` | react / router / radix 依赖 chunk |
| 其他 `*.js` | 按路由拆分的页面 chunk（任务、告警、总览、源、Worker、任务详情）。任务表有期望列 `desired_run`（`task-desired-<id>`），详情有期望状态（`task-desired-run`）。任务详情复制的多文件回放命令以 `set -o pipefail` 开头，并带 `--verify-binlog-checksum`，`mariadb-binlog` 另带 `--skip-gtid-strict-mode`，注释要求同一 shell 管道并核对行数或 GTID |
| `*.css` | Tailwind 样式 |

## Exports

- 作为 `/ui/assets/` 被浏览器按需加载，与 `internal/ui/static/index.html` 成对发布。
- `make ui-build` 用字节拼接写入 L3 头，不改 minified 正文，正文没变的 chunk 保留原文件；随后 `scripts/check-ui-bundle.sh` 检查整个 import 图的 ESM 语法和 Console 标记。

## Dependencies

- Upstream: console 构建输出。
- Downstream: 浏览器资源请求。

## Update Rule

- 资源组织方式变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
