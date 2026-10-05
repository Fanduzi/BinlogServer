#!/usr/bin/env bash
# input: mysql80 with GTID on, the suite API, and the task data directory
# output: a GTID task whose checkpoint gtid_set includes every committed transaction, then resumes to RUNNING after PURGE BINARY LOGS and stores the later events
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

# uuid_interval_end reads only the intervals that follow server_uuid.
# The largest digit run in the whole string is inside the UUID
# (e816d312-...:1-29 → 816) and does not say whether the set grew.
uuid_interval_end() {
  local uuid="$1"
  local set="$2"
  local flat spec n
  flat="$(printf '%s' "$set" | tr -d ' \r\n')"
  case "$flat" in
    *"${uuid}:"*) ;;
    *)
      printf '0'
      return
      ;;
  esac
  spec="${flat#*"${uuid}:"}"
  spec="${spec%%,*}"
  n="$(printf '%s' "$spec" | grep -oE '[0-9]+' | sort -n | tail -1 || true)"
  printf '%s' "${n:-0}"
}

gtid_subset() {
  local inner="$1"
  local outer="$2"
  inner="$(printf '%s' "$inner" | tr -d ' \r\n')"
  outer="$(printf '%s' "$outer" | tr -d ' \r\n')"
  mysql80 "SELECT GTID_SUBSET('${inner}', '${outer}');"
}

assert_interval_parser() {
  local uuid="e816d312-c0d0-11f1-8ad5-c6fdaa0395d0"
  local seed_n ckpt_n
  seed_n="$(uuid_interval_end "$uuid" "${uuid}:1-28")"
  ckpt_n="$(uuid_interval_end "$uuid" "${uuid}:1-31")"
  if [[ "$seed_n" != "28" || "$ckpt_n" != "31" ]]; then
    echo "gtid interval parser failed: seed=$seed_n checkpoint=$ckpt_n" >&2
    exit 1
  fi
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

assert_interval_parser

echo "[gtid-purge] compression off so GTID and XID stay outside a transaction payload"
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"
mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('gtid-purge-seed-${RUN_TAG}');"
server_uuid="$(mysql80 "SELECT @@GLOBAL.server_uuid;")"
seed="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
seed_end="$(uuid_interval_end "$server_uuid" "$seed")"
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

# Three autocommit inserts. A checkpoint that only records the first one
# is still behind GTID_EXECUTED and must fail.
for n in 1 2 3; do
  mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('gtid-purge-grow-${RUN_TAG}-${n}');"
done
executed="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
exec_end="$(uuid_interval_end "$server_uuid" "$executed")"
if [[ "$exec_end" -lt $((seed_end + 3)) ]]; then
  echo "source GTID_EXECUTED did not gain 3 transactions: seed=$seed executed=$executed" >&2
  exit 1
fi
ckpt_file=""
ckpt_gtid=""
for _ in $(seq 1 60); do
  cp="$(curl -fsS "$API/api/tasks/$task_id/checkpoint")"
  ckpt_gtid="$(printf '%s' "$cp" | jq -r '.gtid_set // empty')"
  ckpt_file="$(printf '%s' "$cp" | jq -r '.file // empty')"
  ckpt_end="$(uuid_interval_end "$server_uuid" "$ckpt_gtid")"
  if [[ -n "$ckpt_gtid" && "$ckpt_end" -ge "$exec_end" && "$(gtid_subset "$executed" "$ckpt_gtid")" == "1" ]]; then
    break
  fi
  ckpt_gtid=""
  sleep 0.5
done
if [[ -z "$ckpt_gtid" ]]; then
  echo "checkpoint gtid does not include executed $executed (seed $seed): $(curl -fsS "$API/api/tasks/$task_id/checkpoint")" >&2
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
mysql80 "INSERT INTO binlog_e2e_80.t1(v) VALUES('gtid-purge-after-${RUN_TAG}');"
executed="$(mysql80 "SELECT @@GLOBAL.GTID_EXECUTED;" | tr -d ' \n')"
exec_end="$(uuid_interval_end "$server_uuid" "$executed")"
if [[ "$exec_end" -lt $((ckpt_end + 2)) ]]; then
  echo "source GTID_EXECUTED did not gain the post-resume transactions: before=$ckpt_gtid executed=$executed" >&2
  exit 1
fi
found=0
resumed_gtid=""
for _ in $(seq 1 60); do
  cp="$(curl -fsS "$API/api/tasks/$task_id/checkpoint")"
  resumed_gtid="$(printf '%s' "$cp" | jq -r '.gtid_set // empty')"
  if [[ -n "$resumed_gtid" && "$(uuid_interval_end "$server_uuid" "$resumed_gtid")" -ge "$exec_end" && "$(gtid_subset "$executed" "$resumed_gtid")" == "1" ]]; then
    if grep -a -R -F -q "$MARKER" "$DATA_DIR/$task_id" 2>/dev/null; then
      found=1
      break
    fi
  fi
  sleep 0.5
done
if [[ "$found" != "1" ]]; then
  echo "post-resume checkpoint does not include $executed or marker $MARKER is missing under $DATA_DIR/$task_id: $(curl -fsS "$API/api/tasks/$task_id/checkpoint")" >&2
  exit 1
fi
echo "[gtid-purge] success: gtid=$resumed_gtid includes every commit, and the purged file resumed by GTID"
