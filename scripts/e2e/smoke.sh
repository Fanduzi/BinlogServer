#!/usr/bin/env bash
# input: local tooling, e2e services, and the canonical E2E database topology
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="http://127.0.0.1:18080"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

# 创建任务后立即 start，返回 task id 供后续轮询和检查。
create_task() {
  local name="$1"
  local port="$2"
  local sid="$3"

  local resp
  resp=$(curl -sS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"cluster_key\":\"$name\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":$port,\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":$sid},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")

  local id
  id=$(printf '%s' "$resp" | jq -r '.id // empty')

  if [[ -z "$id" || "$id" == "null" ]]; then
    echo "create task failed: $resp" >&2
    exit 1
  fi

  curl -sS -X POST "$API/api/tasks/$id/start" >/dev/null
  echo "$id"
}

wait_running() {
  local id="$1"
  # 等待任务进入 RUNNING，避免后续写入发生在拉流尚未建立时。
  for _ in {1..30}; do
    local st
    st=$(curl -sS "$API/api/tasks/$id" | jq -r '.state // empty')
    if [[ "$st" == "RUNNING" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "task $id not RUNNING in time" >&2
  return 1
}

echo "[e2e] create + start tasks"
# 每个 source 使用固定唯一 server_id，避免 replication client 冲突。
id57=$(create_task "e2e-mysql57" "$E2E_MYSQL57_PORT" 310101)
id80=$(create_task "e2e-mysql80" "$E2E_MYSQL80_PORT" 310102)
idp57=$(create_task "e2e-percona57" "$E2E_PERCONA57_PORT" 310103)
idp80=$(create_task "e2e-percona80" "$E2E_PERCONA80_PORT" 310104)

wait_running "$id57"
wait_running "$id80"
wait_running "$idp57"
wait_running "$idp80"

echo "[e2e] write source data"
# 向 4 个 source 写入数据，触发可观测的 binlog event。
docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot -e "INSERT INTO binlog_e2e_57.t1(v) VALUES('from-57-1'),('from-57-2');"
docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -e "INSERT INTO binlog_e2e_80.t1(v) VALUES('from-80-1'),('from-80-2');"
docker compose -f "$COMPOSE_FILE" exec -T percona57 mysql -uroot -proot -e "INSERT INTO binlog_e2e_percona57.t1(v) VALUES('from-percona57-1'),('from-percona57-2');"
docker compose -f "$COMPOSE_FILE" exec -T percona80 mysql -uroot -proot -e "INSERT INTO binlog_e2e_percona80.t1(v) VALUES('from-percona80-1'),('from-percona80-2');"

sleep 1

echo "[e2e] checkpoints"
# checkpoint 用于确认拉流侧已经推进位点。
echo "mysql57 task $id57 -> $(curl -sS "$API/api/tasks/$id57/checkpoint")"
echo "mysql80 task $id80 -> $(curl -sS "$API/api/tasks/$id80/checkpoint")"
echo "percona57 task $idp57 -> $(curl -sS "$API/api/tasks/$idp57/checkpoint")"
echo "percona80 task $idp80 -> $(curl -sS "$API/api/tasks/$idp80/checkpoint")"

echo "[e2e] done"
