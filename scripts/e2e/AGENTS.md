# scripts/e2e AGENTS

## Members
- `run-suite.sh`: 套件总控。
- `up.sh` / `down.sh`: 环境拉起与回收。
- `run-server.sh`: 后端进程启动。
- `smoke-*.sh`: 场景脚本。
- `setup-meta-replication.sh`: meta failover 场景准备。
- `lib-migration.sh`: e2e 迁移辅助。

## Interfaces
- `make e2e-quick` / `make e2e-full` / `make e2e SCENARIOS=...`。

## Dependencies
- Upstream: Makefile/开发者命令。
- Downstream: docker compose, curl/jq/go, 本地服务端口。

## Update Rule
- 场景行为、环境依赖、执行入口变化时，更新本文件。
