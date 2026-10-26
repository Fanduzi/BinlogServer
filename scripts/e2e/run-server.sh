#!/usr/bin/env bash
# input: local tooling (docker/curl/jq/go) and e2e environment/service dependencies
# output: deterministic e2e orchestration, scenario execution, and verification logs; the test binary is rebuilt up to 3 times when a module fetch fails, then the process execs it
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
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
# 关闭 CGO，避免 Linux 环境的测试二进制绑定构建机 glibc 版本。
# proxy.golang.org 偶发 HTTP/2 INTERNAL_ERROR，一次失败进程就起不来，
# /healthz 要空等 120s。重试这次构建，成功才 exec。
build_ok=0
for attempt in 1 2 3; do
  if CGO_ENABLED=0 go build -o "$BIN_PATH" ./cmd/binlog-server; then
    build_ok=1
    break
  fi
  echo "[e2e] go build failed (attempt ${attempt}/3)" >&2
done
if [[ "$build_ok" -ne 1 ]]; then
  echo "[e2e] go build failed after 3 attempts" >&2
  exit 1
fi
exec "$BIN_PATH" --config "$CONFIG_PATH"
