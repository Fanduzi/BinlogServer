# internal/logging Module

## Files
- `logger.go`: zap 初始化、轮转与重定向。
- `logger_test.go`: 日志初始化测试。

## Exports
- `Setup(ctx, cfg)`：初始化全局日志。

## Dependencies
- Upstream: `cmd/binlog-server`。
- Downstream: `zap`, `lumberjack`。

## Update Rule
- 日志格式、输出、轮转策略变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
