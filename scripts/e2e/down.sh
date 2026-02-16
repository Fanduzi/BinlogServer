#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"

cd "$ROOT_DIR"
# 清理容器 + volume，确保下次 e2e 从干净状态启动。
docker compose -f "$COMPOSE_FILE" down -v
