# cmd/migrate AGENTS

## Members
- `main.go`: 迁移命令定义与执行。
- `main_test.go`: 迁移命令核心逻辑测试。

## Interfaces
- 命令：`up`, `down --steps`, `version`, `force`, `goto`。
- 参数：`--dsn`, `--path`, `--env`, `--allow-destructive`。

## Dependencies
- Upstream: 运维脚本/Makefile 调用。
- Downstream: `migrations/`, `golang-migrate` 驱动。

## Update Rule
- 迁移命令行为、生产保护策略变化时，更新本文件。
