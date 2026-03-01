#!/usr/bin/env bash
# input: local tooling (docker/curl/jq/go) and e2e environment/service dependencies
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"

# 这里只拉起 source MySQL，避免把按需场景（如 orchestrator）也默认启动。
services=(mysql57 mysql80 percona57 percona80)

cd "$ROOT_DIR"
docker compose -f "$COMPOSE_FILE" up -d "${services[@]}"

for svc in "${services[@]}"; do
  echo "[e2e] waiting for $svc ..."
  for i in {1..60}; do
    # 用容器内 mysqladmin 做健康探活，确保实例已可接受连接。
    if docker compose -f "$COMPOSE_FILE" exec -T "$svc" mysqladmin ping -h127.0.0.1 -proot >/dev/null 2>&1; then
      echo "[e2e] $svc is ready"
      break
    fi
    if [[ $i -eq 60 ]]; then
      echo "[e2e] $svc not ready in time" >&2
      exit 1
    fi
    sleep 2
  done
done

echo "[e2e] all sources ready"
