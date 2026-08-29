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
SOURCE_SERVICE="mysql80"
SOURCE_PORT="$E2E_MYSQL80_PORT"
SEMISYNC_TIMEOUT_MS="${SEMISYNC_TIMEOUT_MS:-7000}"
task_id=""
mode=""

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

mysql_exec() {
  local sql="$1"
  docker compose -f "$COMPOSE_FILE" exec -T "$SOURCE_SERVICE" env MYSQL_PWD=root mysql -uroot -Nse "$sql"
}

mysql_try() {
  local sql="$1"
  docker compose -f "$COMPOSE_FILE" exec -T "$SOURCE_SERVICE" env MYSQL_PWD=root mysql -uroot -Nse "$sql" >/dev/null 2>&1 || true
}

json_get_str() {
  local json="$1"
  local lower_key="$2"
  local upper_key="$3"
  printf '%s' "$json" | jq -r ".${lower_key} // .${upper_key} // empty"
}

status_value() {
  local name="$1"
  mysql_exec "SHOW STATUS LIKE '$name';" | awk '{print $2}' | tail -n 1 | tr -d '\r'
}

wait_running() {
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

wait_stopped() {
  local id="$1"
  for _ in {1..120}; do
    local resp st
    resp=$(curl -sS "$API/api/tasks/$id")
    st=$(json_get_str "$resp" "state" "State")
    if [[ "$st" == "STOPPED" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "task $id not STOPPED in time" >&2
  return 1
}

install_semisync_plugins() {
  # go-mysql 当前半同步探测使用 rpl_semi_sync_master_* 变量。
  # 为保证 e2e 覆盖到真实 ACK 路径，这里强制使用 legacy master/slave 插件命名。
  mysql_try "UNINSTALL PLUGIN rpl_semi_sync_source;"
  mysql_try "UNINSTALL PLUGIN rpl_semi_sync_replica;"
  mysql_try "INSTALL PLUGIN rpl_semi_sync_master SONAME 'semisync_master.so';"
  mysql_try "INSTALL PLUGIN rpl_semi_sync_slave SONAME 'semisync_slave.so';"
}

detect_mode_and_enable() {
  local has_source has_master
  has_source="$(mysql_exec "SHOW VARIABLES LIKE 'rpl_semi_sync_source_enabled';" | wc -l | tr -d ' ')"
  has_master="$(mysql_exec "SHOW VARIABLES LIKE 'rpl_semi_sync_master_enabled';" | wc -l | tr -d ' ')"

  if [[ "$has_master" -ge 1 ]]; then
    echo "master"
    mysql_exec "SET GLOBAL rpl_semi_sync_master_enabled = ON;"
    mysql_exec "SET GLOBAL rpl_semi_sync_master_timeout = ${SEMISYNC_TIMEOUT_MS};"
    mysql_exec "SET GLOBAL rpl_semi_sync_master_wait_no_slave = ON;"
    mysql_exec "SET GLOBAL rpl_semi_sync_master_wait_for_slave_count = 1;"
    return 0
  fi

  if [[ "$has_source" -ge 1 ]]; then
    # 兜底：如果环境只有 source/replica 命名，仍尽量启用。
    echo "source"
    mysql_exec "SET GLOBAL rpl_semi_sync_source_enabled = ON;"
    mysql_exec "SET GLOBAL rpl_semi_sync_source_timeout = ${SEMISYNC_TIMEOUT_MS};"
    mysql_exec "SET GLOBAL rpl_semi_sync_source_wait_no_replica = ON;"
    mysql_exec "SET GLOBAL rpl_semi_sync_source_wait_for_replica_count = 1;"
    return 0
  fi

  echo "unknown"
  return 1
}

wait_semisync_client_on() {
  local clients_var="$1"
  local status_var="$2"
  for _ in {1..60}; do
    local clients status
    clients="$(status_value "$clients_var")"
    status="$(status_value "$status_var")"
    if [[ -n "$clients" && "$clients" -ge 1 ]] && [[ "$status" == "ON" || "$status" == "1" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "semi-sync client not active in time" >&2
  echo "clients=$clients_var -> $(status_value "$clients_var")" >&2
  echo "status=$status_var -> $(status_value "$status_var")" >&2
  return 1
}

wait_semisync_client_off() {
  local clients_var="$1"
  for _ in {1..60}; do
    local clients
    clients="$(status_value "$clients_var")"
    if [[ -n "$clients" && "$clients" -eq 0 ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "semi-sync client did not drop to 0 in time" >&2
  echo "clients=$clients_var -> $(status_value "$clients_var")" >&2
  return 1
}

create_semisync_task() {
  local name="$1"
  local sid="$2"
  local resp id
  resp=$(curl -sS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"cluster_key\":\"$name\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":$SOURCE_PORT,\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":$sid,\"semi_sync\":true},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")
  id=$(json_get_str "$resp" "id" "ID")
  if [[ -z "$id" || "$id" == "null" ]]; then
    echo "create task failed: $resp" >&2
    exit 1
  fi
  curl -sS -X POST "$API/api/tasks/$id/start" >/dev/null
  printf '%s' "$id"
}

measure_insert_latency_us() {
  local tag="$1"
  mysql_exec "SET @t0 = NOW(6); INSERT INTO binlog_e2e_80.t1(v) VALUES('semisync-${tag}'); SELECT TIMESTAMPDIFF(MICROSECOND, @t0, NOW(6));" | tail -n 1 | tr -d '\r'
}

cleanup() {
  if [[ -n "$task_id" ]]; then
    curl -sS -X POST "$API/api/tasks/$task_id/stop" >/dev/null 2>&1 || true
  fi
  if [[ "$mode" == "source" ]]; then
    mysql_try "SET GLOBAL rpl_semi_sync_source_enabled = OFF;"
  elif [[ "$mode" == "master" ]]; then
    mysql_try "SET GLOBAL rpl_semi_sync_master_enabled = OFF;"
  fi
}
trap cleanup EXIT

echo "[semisync] install + enable source semi-sync"
install_semisync_plugins
mode="$(detect_mode_and_enable)"
if [[ "$mode" == "unknown" ]]; then
  echo "cannot detect semi-sync variable namespace on source" >&2
  exit 1
fi

if [[ "$mode" == "source" ]]; then
  clients_var="Rpl_semi_sync_source_clients"
  status_var="Rpl_semi_sync_source_status"
else
  clients_var="Rpl_semi_sync_master_clients"
  status_var="Rpl_semi_sync_master_status"
fi

echo "[semisync] mode=$mode timeout_ms=$SEMISYNC_TIMEOUT_MS"
echo "[semisync] create task with semi_sync=true"
run_tag="$(date +%s)"
sid="$((330000 + (run_tag % 1000)))"
task_id="$(create_semisync_task "e2e-semisync-$run_tag" "$sid")"
wait_running "$task_id"
echo "[semisync] task running: $task_id"

wait_semisync_client_on "$clients_var" "$status_var"
echo "[semisync] client attached: $clients_var=$(status_value "$clients_var"), $status_var=$(status_value "$status_var")"

echo "[semisync] stop task and wait detached"
curl -sS -X POST "$API/api/tasks/$task_id/stop" >/dev/null
wait_stopped "$task_id"
wait_semisync_client_off "$clients_var"
echo "[semisync] client detached: $clients_var=$(status_value "$clients_var")"

echo "[semisync] write one tx after stop; expect commit blocked near timeout"
latency_us="$(measure_insert_latency_us "$run_tag")"
if ! [[ "$latency_us" =~ ^[0-9]+$ ]]; then
  echo "invalid latency value: $latency_us" >&2
  exit 1
fi

threshold_us=$(( (SEMISYNC_TIMEOUT_MS - 1000) * 1000 ))
if [[ "$threshold_us" -lt 1000 ]]; then
  threshold_us=1000
fi

echo "[semisync] commit_latency_us=$latency_us threshold_us=$threshold_us"
if [[ "$latency_us" -lt "$threshold_us" ]]; then
  echo "commit not blocked as expected (latency too low)" >&2
  exit 1
fi

echo "[semisync] success: stop semi-sync task can block source commit until timeout"
