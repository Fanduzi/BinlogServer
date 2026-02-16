#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="http://127.0.0.1:18080"
ORC_API="http://127.0.0.1:13000/api"
DISCOVER_HOST="mysql80"
DISCOVER_PORT="3306"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

json_array_len() {
  local json="$1"
  printf '%s' "$json" | jq -r 'length'
}

json_get_str() {
  local json="$1"
  local lower_key="$2"
  local upper_key="$3"
  printf '%s' "$json" | jq -r ".${lower_key} // .${upper_key} // empty"
}

wait_orchestrator() {
  # 等待 orchestrator API ready，避免后续 discover 调用抖动失败。
  for _ in {1..60}; do
    if curl -fsS "$ORC_API/status" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "orchestrator not ready in time" >&2
  return 1
}

discover_once() {
  curl -fsS "$ORC_API/discover/$DISCOVER_HOST/$DISCOVER_PORT" >/dev/null
}

cluster_json() {
  curl -fsS "$ORC_API/cluster/instance/$DISCOVER_HOST/$DISCOVER_PORT"
}

cluster_count() {
  local resp
  resp="$(cluster_json)"
  json_array_len "$resp"
}

wait_cluster_non_empty() {
  # 先确保 discover 至少拿到主库节点，作为后续对比基线。
  for _ in {1..60}; do
    discover_once
    local c
    c="$(cluster_count)"
    if [[ "$c" -ge 1 ]]; then
      return 0
    fi
    sleep 1
  done
  echo "cluster remains empty after discover" >&2
  return 1
}

cluster_hosts_line() {
  local resp
  resp="$(cluster_json)"
  printf '%s' "$resp" | jq -r 'map(.Key.Hostname + ":" + (.Key.Port|tostring)) | join(",")'
}

create_task() {
  local task_name
  task_name="e2e-orchestrator-mysql80-$(date +%s)"
  local resp
  # 创建一个 LATEST 起点任务，使 binlog_server 与 source 建立 dump 连接。
  resp=$(curl -sS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$task_name\",\"source\":{\"host\":\"127.0.0.1\",\"port\":13307,\"user\":\"repl\",\"password\":\"replpass\",\"flavor\":\"mysql\",\"server_id\":330901},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")
  local id
  id=$(json_get_str "$resp" "id" "ID")
  if [[ -z "$id" || "$id" == "null" ]]; then
    echo "create task failed: $resp" >&2
    exit 1
  fi
  curl -sS -X POST "$API/api/tasks/$id/start" >/dev/null
  printf '%s' "$id"
}

wait_task_running() {
  local id="$1"
  for _ in {1..60}; do
    local resp st
    resp=$(curl -sS "$API/api/tasks/$id")
    st=$(json_get_str "$resp" "state" "State")
    if [[ "$st" == "RUNNING" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "task $id not RUNNING in time" >&2
  return 1
}

echo "[orchestrator] start service"
docker compose -f "$COMPOSE_FILE" up -d orchestrator
wait_orchestrator

echo "[orchestrator] discover source"
wait_cluster_non_empty
before_count="$(cluster_count)"
before_hosts="$(cluster_hosts_line)"
echo "[orchestrator] before: count=$before_count hosts=$before_hosts"

# 预期基线只包含 source 主库一个节点。
if [[ "$before_count" -ne 1 ]]; then
  echo "unexpected baseline cluster size: $before_count (expect 1)" >&2
  cluster_json >&2
  exit 1
fi

echo "[orchestrator] start binlog task"
task_id="$(create_task)"
wait_task_running "$task_id"
echo "[orchestrator] task running: $task_id"

echo "[orchestrator] write data to keep binlog dump active"
# 主动写入一条数据，保证拉流连接有实际 event 流动。
docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot \
  -e "INSERT INTO binlog_e2e_80.t1(v) VALUES('orchestrator-e2e-$(date +%s)');"

echo "[orchestrator] re-check topology after waiting"
sleep 12
discover_once
sleep 5

after_count="$(cluster_count)"
after_hosts="$(cluster_hosts_line)"
echo "[orchestrator] after: count=$after_count hosts=$after_hosts"

if [[ "$after_count" -ne 1 ]]; then
  # 如果节点数增加，说明 binlog client 有被纳入拓扑的风险。
  echo "topology expanded unexpectedly after binlog_server start" >&2
  cluster_json >&2
  exit 1
fi

echo "[orchestrator] success: topology still has only source primary node"
