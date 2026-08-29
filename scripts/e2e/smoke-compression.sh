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
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

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

create_task_file_pos() {
  local name="$1"
  local port="$2"
  local sid="$3"
  local file="$4"
  local pos="$5"

  local resp
  # 从 FILE/POS 起点拉取，确保我们明确验证“一个完整 binlog 文件”的一致性。
  resp=$(curl -sS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"cluster_key\":\"$name\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":$port,\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":$sid},\"start\":{\"mode\":\"FILE_POS\",\"file\":\"$file\",\"pos\":$pos},\"storage\":{\"retention_days\":7}}")

  local id
  id=$(json_get_str "$resp" "id" "ID")
  if [[ -z "$id" || "$id" == "null" ]]; then
    echo "create task failed: $resp" >&2
    exit 1
  fi
  curl -sS -X POST "$API/api/tasks/$id/start" >/dev/null
  echo "$id"
}

wait_running() {
  local id="$1"
  for _ in {1..60}; do
    local st
    local resp
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

get_checkpoint_file() {
  local id="$1"
  local resp
  resp=$(curl -sS "$API/api/tasks/$id/checkpoint")
  json_get_str "$resp" "file" "File"
}

get_checkpoint_pos() {
  local id="$1"
  local resp
  resp=$(curl -sS "$API/api/tasks/$id/checkpoint")
  json_get_num "$resp" "pos" "Pos"
}

wait_rotated() {
  local id="$1"
  local from_file="$2"
  # 轮询 checkpoint file 是否切换，作为 rotate 已发生的判据。
  for _ in {1..120}; do
    local file
    file=$(get_checkpoint_file "$id")
    if [[ -n "$file" && "$file" != "$from_file" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "task $id did not rotate from $from_file in time" >&2
  return 1
}

assert_compression_on() {
  local svc="$1"
  local value
  value=$(docker compose -f "$COMPOSE_FILE" exec -T "$svc" mysql -uroot -proot -Nse "SELECT @@GLOBAL.binlog_transaction_compression;" | tr -d '\r')
  if [[ "$value" != "ON" && "$value" != "1" ]]; then
    echo "$svc binlog_transaction_compression is not ON: $value" >&2
    exit 1
  fi
}

master_file() {
  local svc="$1"
  docker compose -f "$COMPOSE_FILE" exec -T "$svc" mysql -uroot -proot -Nse "SHOW MASTER STATUS" | awk '{print $1}' | tr -d '\r'
}

flush_logs() {
  local svc="$1"
  docker compose -f "$COMPOSE_FILE" exec -T "$svc" mysql -uroot -proot -e "FLUSH BINARY LOGS;"
}

write_large_tx() {
  local svc="$1"
  local db="$2"
  local tag="$3"
  local sql
  sql="$(mktemp)"
  local payload
  # 构造较大的事务 payload，提升触发 compressed transaction 的概率。
  payload="$(printf 'x%.0s' $(seq 1 512))"

  {
    echo "BEGIN;"
    for i in $(seq 1 200); do
      printf "INSERT INTO %s.t1(v) VALUES('%s-%03d-%s');\n" "$db" "$tag" "$i" "$payload"
    done
    echo "COMMIT;"
  } >"$sql"

  docker compose -f "$COMPOSE_FILE" exec -T "$svc" mysql -uroot -proot <"$sql"
  rm -f "$sql"
}

md5_local() {
  local file="$1"
  if command -v md5sum >/dev/null 2>&1; then
    md5sum "$file" | awk '{print $1}'
    return
  fi
  if command -v md5 >/dev/null 2>&1; then
    md5 -q "$file"
    return
  fi
  echo "missing md5 tool (md5sum/md5)" >&2
  exit 1
}

md5_in_container() {
  local svc="$1"
  local file="$2"
  docker compose -f "$COMPOSE_FILE" exec -T "$svc" sh -lc '
set -e
f="$1"
if command -v md5sum >/dev/null 2>&1; then
  md5sum "$f" | awk "{print \$1}"
elif command -v md5 >/dev/null 2>&1; then
  md5 -q "$f"
else
  openssl md5 "$f" | sed "s/^.*= //"
fi
' sh "/var/lib/mysql/$file" | tr -d '\r'
}

verify_one_source() {
  local svc="$1"
  local port="$2"
  local sid="$3"
  local db="$4"
  local tag="$5"

  echo "[compression] check variable on $svc"
  assert_compression_on "$svc"

  echo "[compression] prepare clean rotate on $svc"
  # 先 rotate 一次，得到干净的起始 binlog 文件，减少历史干扰。
  flush_logs "$svc"
  local start_file
  start_file="$(master_file "$svc")"
  if [[ -z "$start_file" ]]; then
    echo "cannot get master file from $svc" >&2
    exit 1
  fi

  local task_id
  task_id="$(create_task_file_pos "e2e-compress-$svc-$tag" "$port" "$sid" "$start_file" 4)"
  wait_running "$task_id"
  local before_pos
  before_pos="$(get_checkpoint_pos "$task_id")"
  echo "[compression] $svc task=$task_id start_file=$start_file before_pos=${before_pos:-N/A}"

  echo "[compression] write large transaction on $svc"
  write_large_tx "$svc" "$db" "$tag"

  echo "[compression] flush binary logs on $svc (force rotate)"
  # 再次 rotate，强制封口 start_file，便于做源端/备份文件 md5 对比。
  flush_logs "$svc"
  wait_rotated "$task_id" "$start_file"

  local backup_file="$DATA_DIR/$task_id/$start_file"
  for _ in {1..60}; do
    if [[ -f "$backup_file" ]]; then
      break
    fi
    sleep 0.5
  done
  if [[ ! -f "$backup_file" ]]; then
    echo "backup file not found: $backup_file" >&2
    exit 1
  fi

  local src_md5
  local bak_md5
  src_md5="$(md5_in_container "$svc" "$start_file")"
  bak_md5="$(md5_local "$backup_file")"
  echo "[compression] $svc md5 src=$src_md5 bak=$bak_md5 file=$start_file"
  if [[ "$src_md5" != "$bak_md5" ]]; then
    # md5 不一致直接失败：说明落盘文件字节级别和源端不一致。
    echo "md5 mismatch for $svc file $start_file" >&2
    exit 1
  fi
}

run_tag="$(date +%s)"

# 只对 8.0 系列做压缩事务专项（5.7 不支持该特性）。
verify_one_source "mysql80" "$E2E_MYSQL80_PORT" "320801" "binlog_e2e_80" "$run_tag"
verify_one_source "percona80" "$E2E_PERCONA80_PORT" "320802" "binlog_e2e_percona80" "$run_tag"

echo "[compression] success: mysql80/percona80 compressed transaction replicated and md5 matched"
