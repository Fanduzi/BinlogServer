#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

# 默认准备本地数据目录；CI 可通过 BINLOG_SERVER_DATA_DIR 覆盖到独立路径。
mkdir -p tmp/e2e/data
# 使用 e2e 专用配置启动后端，便于脚本固定访问 127.0.0.1:18080。
exec go run ./cmd/binlog-server --config deploy/e2e/config.yaml
