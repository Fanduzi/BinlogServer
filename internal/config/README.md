# internal/config Module

## Files
- `config.go`: 配置模型、加载与默认值。
- `config_test.go`: 配置加载与覆盖规则测试。

## Exports
- `LoadConfig(path)`：加载配置。
- 环境变量覆盖规则与占位符展开。

## Dependencies
- Upstream: `cmd/binlog-server`, `internal/app`。
- Downstream: `viper`, process env。

## Update Rule
- 配置项、默认值、覆盖规则变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
