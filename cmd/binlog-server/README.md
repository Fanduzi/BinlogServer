# cmd/binlog-server Module

## Files
- `main.go`: 进程入口。
- `cmd/root.go`: Cobra root command，负责配置加载与应用启动。

## Exports
- CLI 接口：`binlog-server --config <path>`；`--encryption-key` 同时解密 `enc:aes256:` 配置值并加密 meta 中的源库密码。
- 运行入口：执行 app 运行时。

## Dependencies
- Upstream: shell/system process manager。
- Downstream: `internal/config`, `internal/logging`, `internal/app`。

## Update Rule
- 入口参数、启动流程、信号处理变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
