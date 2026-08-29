#!/usr/bin/env bash
# input: Docker and the canonical E2E database topology
# output: removed E2E containers and volumes
# pos: E2E environment teardown adapter
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"

cd "$ROOT_DIR"
# 清理容器 + volume，确保下次 e2e 从干净状态启动。
docker compose -f "$COMPOSE_FILE" down -v
