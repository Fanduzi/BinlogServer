#!/usr/bin/env bash
# input: mysql80 with GTID on, the suite API, and the task data directory
# output: a GTID task whose checkpoint gtid_set grows, then resumes to RUNNING after PURGE BINARY LOGS and stores a new event
# pos: docker coverage for raw-mode GTID checkpoints and a purged-file resume whose 1236 arrives on the event stream
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data}"
RUN_TAG="$(date +%s)"
MARKER="gtid-purge-${RUN_TAG}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

mysql80() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -Nse "$1" | tr -d '\r'
}

restore_compression() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -e "SET GLOBAL binlog_transaction_compression=ON;" >/dev/null 2>&1 || true
}
trap restore_compression EXIT

interval_end() {
  local n
  n="$(printf '%s' "$1" | grep -oE '[0-9]+' | sort -n | tail -1 || true)"
  printf '%s' "${n:-0}"
}

task_json() {
  curl -fsS "$API/api/tasks/$1"
}

wait_state() {
  local id="$1"
  local want="$2"
  local i resp st
  for i in $(seq 1 60); do
    resp="$(task_json "$id")"
    st="$(printf '%s' "$resp" | jq -r '.state // empty')"
    if [[ "$st" == "$want" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "task $id did not reach $want: $(task_json "$id")" >&2
  return 1
}

echo "[gtid-purge] compression off so GTID and XID stay outside a transaction payload"
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"
mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('gtid-purge-seed-${RUN_TAG}');"
seed="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;")"
seed_end="$(interval_end "$seed")"
if [[ -z "$seed" || "$seed_end" == "0" ]]; then
  echo "empty GTID_EXECUTED: $seed" >&2
  exit 1
fi
echo "[gtid-purge] seed $seed"

name="e2e-gtid-purge-${RUN_TAG}"
body="$(jq -n \
  --arg name "$name" \
  --arg gtid "$seed" \
  --arg host "$E2E_SOURCE_HOST" \
  --arg user "$E2E_SOURCE_USER" \
  --arg pass "$E2E_SOURCE_PASS" \
  --argjson port "$E2E_MYSQL80_PORT" \
  '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:310180},start:{mode:"GTID",gtid_set:$gtid},storage:{retention_days:7}}')"
resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
task_id="$(printf '%s' "$resp" | jq -r '.id // empty')"
if [[ -z "$task_id" || "$task_id" == "null" ]]; then
  echo "create task failed: $resp" >&2
  exit 1
fi
curl -fsS -X POST "$API/api/tasks/$task_id/start" >/dev/null
wait_state "$task_id" "RUNNING"
echo "[gtid-purge] task $task_id running"

mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('gtid-purge-grow-${RUN_TAG}');"
ckpt_file=""
ckpt_gtid=""
for _ in $(seq 1 60); do
  cp="$(curl -fsS "$API/api/tasks/$task_id/checkpoint")"
  ckpt_gtid="$(printf '%s' "$cp" | jq -r '.gtid_set // empty')"
  ckpt_file="$(printf '%s' "$cp" | jq -r '.file // empty')"
  if [[ -n "$ckpt_gtid" && "$(interval_end "$ckpt_gtid")" -gt "$seed_end" ]]; then
    break
  fi
  ckpt_gtid=""
  sleep 0.5
done
if [[ -z "$ckpt_gtid" ]]; then
  echo "checkpoint gtid did not grow past $seed: $(curl -fsS "$API/api/tasks/$task_id/checkpoint")" >&2
  exit 1
fi
echo "[gtid-purge] checkpoint file=$ckpt_file gtid=$ckpt_gtid"
if [[ "$ckpt_file" != mysql-bin.* ]]; then
  echo "checkpoint file is not a source binlog: $ckpt_file" >&2
  exit 1
fi

curl -fsS -X POST "$API/api/tasks/$task_id/stop" >/dev/null
wait_state "$task_id" "STOPPED"

mysql80 "FLUSH BINARY LOGS;"
newest="$(mysql80 "SHOW BINARY LOGS;" | awk 'END { print $1 }')"
if [[ -z "$newest" || "$newest" == "$ckpt_file" ]]; then
  echo "flush did not open a file after $ckpt_file (newest=$newest)" >&2
  exit 1
fi
mysql80 "PURGE BINARY LOGS TO '${newest}';"
if mysql80 "SHOW BINARY LOGS;" | awk '{ print $1 }' | grep -qx "$ckpt_file"; then
  echo "purge left $ckpt_file in the index (to $newest)" >&2
  exit 1
fi
echo "[gtid-purge] purged through $newest; $ckpt_file is gone"

curl -fsS -X POST "$API/api/tasks/$task_id/start" >/dev/null
for _ in $(seq 1 60); do
  resp="$(task_json "$task_id")"
  st="$(printf '%s' "$resp" | jq -r '.state // empty')"
  err="$(printf '%s' "$resp" | jq -r '.last_error // empty')"
  if [[ "$st" == "RUNNING" && "$err" != *1236* ]]; then
    break
  fi
  if [[ "$st" == "RETRY_BACKOFF" || "$st" == "FAILED" || "$err" == *1236* ]]; then
    echo "resume looped on 1236: $resp" >&2
    exit 1
  fi
  st=""
  sleep 0.5
done
if [[ "${st:-}" != "RUNNING" ]]; then
  echo "task did not reach RUNNING after purge: $(task_json "$task_id")" >&2
  exit 1
fi
echo "[gtid-purge] resumed RUNNING"

mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('${MARKER}');"
found=0
for _ in $(seq 1 60); do
  if grep -a -R -F -q "$MARKER" "$DATA_DIR/$task_id" 2>/dev/null; then
    found=1
    break
  fi
  sleep 0.5
done
if [[ "$found" != "1" ]]; then
  echo "marker $MARKER was not captured under $DATA_DIR/$task_id" >&2
  exit 1
fi
echo "[gtid-purge] success: gtid advanced and the purged file resumed by GTID"
