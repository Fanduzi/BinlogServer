# cmd/migrate Module

## Files
- `main.go`: 迁移命令定义与执行。跨进 schema 11 之前 `warnLongTransactions` 数 `information_schema.innodb_trx` 里超 5 秒的事务（数到则拒绝、无 `PROCESS` 只告警）；版本停在脏的 11 时先 down 那一格再接着 up；`liveRegistrationGate` 是 000011 行内除零预检的确定性替代（连接池上 `SET SESSION` 不保同连，除零会退化成 NULL+warning）——跨进 11 拒活注册、离开 11 拒 protocol-11 活注册；拨号失败的错误带 `metaDialErr` 前缀。
- `main_test.go`: 迁移命令核心逻辑测试。

## Exports
- 命令：`up`, `down --steps`, `version`, `force`, `goto`。
- 参数：`--dsn`, `--config`, `--path`, `--env`, `--allow-destructive`。`--config` 读取与进程相同的 `meta.tls` 并改写 DSN。`--dsn` 和 `META_DSN` 覆盖文件里的 `meta_dsn`。不传 `--config` 时 DSN 不变。拨号失败的错误带 `meta.tls` 前缀（`tls_test.go`）。
- Release：归档内提供 `migrate` 可执行文件及 `migrations/*.sql`，无需 Go 工具链即可执行迁移。

## Dependencies
- Upstream: 运维脚本/Makefile 调用。
- Downstream: `migrations/`, `golang-migrate` 驱动。`up` 和跨进版本 11 的 `goto` 先做 `CheckLongTransactions`：没有 `PROCESS` 时向 stderr 警告并继续，看得见超过 5 秒的事务则在 DDL 之前退出。已经在 11 上的再次 `up` 不再警告。000011 的 DDL 可以重跑：`schema_migrations` 在版本 11 上 dirty 时，`up` 和目标不低于 11 的 `goto` 先把版本拨回 10 再应用，不需要手工 `force`。不安装触发器或过程。`liveRegistrationGate` 同样守住 `down`/低位 `goto`：11 上还有未过期 protocol-11 注册时拒绝降级。

## Update Rule
- 迁移命令行为、生产保护策略变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
