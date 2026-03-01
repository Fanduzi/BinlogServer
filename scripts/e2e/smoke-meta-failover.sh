#!/usr/bin/env bash
# input: local tooling (docker/curl/jq/go) and e2e environment/service dependencies
# output: deterministic e2e orchestration, scenario execution, and verification logs
# pos: integration-test automation layer validating end-to-end system behavior
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
ORC_API="${E2E_ORC_API:-http://127.0.0.1:13000/api}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

CHECKPOINT_HTTP_CODE=""
CHECKPOINT_HTTP_BODY=""

api_get() {
  local path="$1"
  curl -fsS "$API$path"
}

api_post() {
  local path="$1"
  curl -fsS -X POST "$API$path"
}

json_get_str() {
  local json="$1"
  local lower_key="$2"
  local upper_key="$3"
  printf '%s' "$json" | jq -r ".${lower_key} // .${upper_key} // empty"
}

json_get_num() {
  local json="$1"
  local lower_key="$2"
  local upper_key="$3"
  printf '%s' "$json" | jq -r ".${lower_key} // .${upper_key} // empty"
}

wait_orchestrator() {
  for _ in {1..90}; do
    if curl -fsS "$ORC_API/status" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "orchestrator not ready in time" >&2
  return 1
}

wait_server_ready() {
  for _ in {1..90}; do
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "binlog-server not ready in time" >&2
  return 1
}

discover_host() {
  local service_name="$1"
  for _ in {1..90}; do
    local resp host
    resp="$(curl -fsS "$ORC_API/discover/$service_name/3306" 2>/dev/null || true)"
    host="$(printf '%s' "$resp" | jq -r '.Details.Key.Hostname // empty' 2>/dev/null || true)"
    if [[ -n "$host" && "$host" != "null" ]]; then
      printf '%s' "$host"
      return 0
    fi
    sleep 1
  done
  echo "discover failed for $service_name" >&2
  return 1
}

cluster_json() {
  local anchor_host="$1"
  curl -fsS "$ORC_API/cluster/instance/$anchor_host/3306"
}

current_writer() {
  local anchor_host="$1"
  cluster_json "$anchor_host" | jq -r '.[] | select(.ReadOnly == false) | .Key.Hostname' | head -n1
}

wait_writer_changed() {
  local anchor_host="$1"
  local old_writer="$2"
  for _ in {1..120}; do
    local writer
    writer="$(current_writer "$anchor_host")"
    if [[ -n "$writer" && "$writer" != "$old_writer" ]]; then
      printf '%s' "$writer"
      return 0
    fi
    sleep 1
  done
  echo "writer did not change from $old_writer in time" >&2
  cluster_json "$anchor_host" >&2 || true
  return 1
}

create_task() {
  local task_name
  task_name="e2e-meta-failover-$(date +%s)"
  local sid
  sid=$((340000 + RANDOM % 20000))
  local resp
  if ! resp="$(curl -fsS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$task_name\",\"cluster_key\":\"$task_name\",\"source\":{\"host\":\"127.0.0.1\",\"port\":13307,\"user\":\"repl\",\"password\":\"replpass\",\"flavor\":\"mysql\",\"server_id\":$sid},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")"; then
    echo "create task request failed" >&2
    exit 1
  fi

  local id
  id="$(json_get_str "$resp" "id" "ID")"
  if [[ -z "$id" || "$id" == "null" ]]; then
    echo "create task failed: $resp" >&2
    exit 1
  fi
  api_post "/api/tasks/$id/start" >/dev/null
  printf '%s' "$id"
}

task_state() {
  local task_id="$1"
  local resp
  if ! resp="$(api_get "/api/tasks/$task_id")"; then
    echo "query task state failed: task_id=$task_id" >&2
    return 1
  fi
  json_get_str "$resp" "state" "State"
}

wait_task_running() {
  local task_id="$1"
  for _ in {1..120}; do
    local st
    if ! st="$(task_state "$task_id")"; then
      return 1
    fi
    if [[ "$st" == "RUNNING" ]]; then
      return 0
    fi
    if [[ "$st" == "FAILED" || "$st" == "STOPPED" ]]; then
      echo "task $task_id entered terminal state: $st" >&2
      api_get "/api/tasks/$task_id" >&2 || true
      return 1
    fi
    sleep 1
  done
  echo "task $task_id not RUNNING in time" >&2
  return 1
}

checkpoint_json() {
  local task_id="$1"
  if ! checkpoint_fetch "$task_id"; then
    return 1
  fi
  if [[ "$CHECKPOINT_HTTP_CODE" != "200" ]]; then
    echo "query checkpoint failed: task_id=$task_id http=$CHECKPOINT_HTTP_CODE body=$CHECKPOINT_HTTP_BODY" >&2
    return 1
  fi
  printf '%s' "$CHECKPOINT_HTTP_BODY"
}

checkpoint_fetch() {
  local task_id="$1"
  local raw
  if ! raw="$(curl -sS -w $'\n%{http_code}' "$API/api/tasks/$task_id/checkpoint")"; then
    echo "query checkpoint request failed: task_id=$task_id" >&2
    return 1
  fi
  CHECKPOINT_HTTP_CODE="${raw##*$'\n'}"
  CHECKPOINT_HTTP_BODY="${raw%$'\n'*}"
}

checkpoint_file() {
  local task_id="$1"
  local resp
  if ! resp="$(checkpoint_json "$task_id")"; then
    echo "query checkpoint file failed: task_id=$task_id" >&2
    return 1
  fi
  json_get_str "$resp" "file" "File"
}

checkpoint_pos() {
  local task_id="$1"
  local resp
  if ! resp="$(checkpoint_json "$task_id")"; then
    echo "query checkpoint pos failed: task_id=$task_id" >&2
    return 1
  fi
  json_get_num "$resp" "pos" "Pos"
}

wait_checkpoint_ready() {
  local task_id="$1"
  for _ in {1..120}; do
    if ! checkpoint_fetch "$task_id"; then
      return 1
    fi
    if [[ "$CHECKPOINT_HTTP_CODE" == "404" ]]; then
      sleep 1
      continue
    fi
    if [[ "$CHECKPOINT_HTTP_CODE" != "200" ]]; then
      echo "checkpoint query failed while waiting ready: task_id=$task_id http=$CHECKPOINT_HTTP_CODE body=$CHECKPOINT_HTTP_BODY" >&2
      return 1
    fi
    local file pos
    file="$(json_get_str "$CHECKPOINT_HTTP_BODY" "file" "File")"
    pos="$(json_get_num "$CHECKPOINT_HTTP_BODY" "pos" "Pos")"
    if [[ -n "$file" && "$file" != "null" && -n "$pos" && "$pos" != "null" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "checkpoint for task $task_id not ready in time" >&2
  return 1
}

write_source_data() {
  local tag="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot \
    -e "INSERT INTO binlog_e2e_80.t1(v) VALUES('${tag}-1'),('${tag}-2'),('${tag}-3');"
}

wait_checkpoint_progress() {
  local task_id="$1"
  local before_file="$2"
  local before_pos="$3"

  for _ in {1..180}; do
    if ! checkpoint_fetch "$task_id"; then
      return 1
    fi
    if [[ "$CHECKPOINT_HTTP_CODE" == "404" ]]; then
      sleep 1
      continue
    fi
    if [[ "$CHECKPOINT_HTTP_CODE" != "200" ]]; then
      echo "checkpoint query failed while waiting progress: task_id=$task_id http=$CHECKPOINT_HTTP_CODE body=$CHECKPOINT_HTTP_BODY" >&2
      return 1
    fi
    local file pos
    file="$(json_get_str "$CHECKPOINT_HTTP_BODY" "file" "File")"
    pos="$(json_get_num "$CHECKPOINT_HTTP_BODY" "pos" "Pos")"
    if [[ -n "$file" && -n "$pos" && "$file" != "null" && "$pos" != "null" ]]; then
      if [[ "$file" != "$before_file" ]] || [[ "$pos" =~ ^[0-9]+$ && "$before_pos" =~ ^[0-9]+$ && "$pos" -gt "$before_pos" ]]; then
        return 0
      fi
    fi
    sleep 1
  done
  echo "checkpoint did not progress after failover, before=${before_file}:${before_pos}" >&2
  local after_json
  after_json="$(checkpoint_json "$task_id" || true)"
  echo "after=${after_json:-<unavailable>}" >&2
  return 1
}

echo "[meta-failover] ensure orchestrator up"
docker compose -f "$COMPOSE_FILE" up -d orchestrator >/dev/null
wait_orchestrator
wait_server_ready

echo "[meta-failover] discover meta topology"
primary_host="$(discover_host "meta-primary")"
replica_host="$(discover_host "meta-replica")"
echo "[meta-failover] discovered primary=$primary_host replica=$replica_host"

cluster_anchor="$primary_host"
writer_before="$(current_writer "$cluster_anchor")"
if [[ -z "$writer_before" ]]; then
  echo "cannot identify writer from orchestrator cluster" >&2
  cluster_json "$cluster_anchor" >&2 || true
  exit 1
fi
echo "[meta-failover] writer before failover=$writer_before"

echo "[meta-failover] create and start task"
task_id="$(create_task)"
wait_task_running "$task_id"
write_source_data "meta-before-$(date +%s)"
wait_checkpoint_ready "$task_id"

before_file="$(checkpoint_file "$task_id")"
before_pos="$(checkpoint_pos "$task_id")"
echo "[meta-failover] checkpoint before failover: ${before_file}:${before_pos}"

echo "[meta-failover] trigger graceful-master-takeover"
curl -fsS "$ORC_API/graceful-master-takeover/$writer_before/3306" >/dev/null
writer_after="$(wait_writer_changed "$cluster_anchor" "$writer_before")"
echo "[meta-failover] writer after failover=$writer_after"

write_source_data "meta-after-$(date +%s)"
wait_task_running "$task_id"
wait_checkpoint_progress "$task_id" "$before_file" "$before_pos"

after_file="$(checkpoint_file "$task_id")"
after_pos="$(checkpoint_pos "$task_id")"
echo "[meta-failover] checkpoint after failover: ${after_file}:${after_pos}"
echo "[meta-failover] success"
