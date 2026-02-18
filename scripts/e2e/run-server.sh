#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

CONFIG_PATH="${BINLOG_SERVER_CONFIG:-deploy/e2e/config.yaml}"
DATA_DIR="${BINLOG_SERVER_DATA_DIR:-tmp/e2e/data}"
BIN_PATH="${BINLOG_SERVER_BIN:-$ROOT_DIR/tmp/e2e/binlog-server-e2e}"

# 默认准备本地数据目录；多实例场景可通过 BINLOG_SERVER_DATA_DIR 指向独立路径。
mkdir -p "$DATA_DIR"
mkdir -p "$(dirname "$BIN_PATH")"
# 使用 e2e 配置启动后端；具体 role/listen_addr 等由 BINLOG_SERVER_* 环境变量覆盖。
go build -o "$BIN_PATH" ./cmd/binlog-server
exec "$BIN_PATH" --config "$CONFIG_PATH"
