# cmd/binlog-server AGENTS

## Members
- `main.go`: 进程入口。
- `cmd/root.go`: Cobra root command，负责配置加载与应用启动。

## Interfaces
- CLI 接口：`binlog-server --config <path>`。
- 运行入口：执行 app 运行时。

## Dependencies
- Upstream: shell/system process manager。
- Downstream: `internal/config`, `internal/logging`, `internal/app`。

## Update Rule
- 入口参数、启动流程、信号处理变化时，更新本文件。
