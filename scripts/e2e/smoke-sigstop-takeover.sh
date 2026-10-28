#!/usr/bin/env bash
# input: canonical E2E database topology (compose mysql57 as source), two workers
#   on per_node storage with separate data_dirs
# output: ADR 0011 PR-4 e2e (production-readiness C3, #473): SIGSTOP the owner
#   past lease_ttl; the peer claims then yields because the chain tail is on the
#   frozen node's disk — the row never becomes FAILED and desired_run never
#   becomes STOP; SIGCONT lets the original owner yield its stale epoch and
#   reclaim under a larger one; the source shows at most one Binlog Dump thread
#   at every sample
# pos: integration-test automation layer validating end-to-end cluster failover
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-sigstop-takeover-$(date +%s)}"
WORKER1_ID="${E2E_WORKER1_ID:-e2e-sigstop-1}"
WORKER2_ID="${E2E_WORKER2_ID:-e2e-sigstop-2}"
WORKER1_HEALTH_ADDR="${E2E_WORKER1_HEALTH_ADDR:-127.0.0.1:18081}"
WORKER2_HEALTH_ADDR="${E2E_WORKER2_HEALTH_ADDR:-127.0.0.1:18082}"

RUN_TAG="$(date +%s)"
CONTROL_LOG="${E2E_CONTROL_LOG:-/tmp/binlog-server-e2e-sigstop-control-${RUN_TAG}.log}"
WORKER1_LOG="${E2E_WORKER1_LOG:-/tmp/binlog-server-e2e-sigstop-worker1-${RUN_TAG}.log}"
WORKER2_LOG="${E2E_WORKER2_LOG:-/tmp/binlog-server-e2e-sigstop-worker2-${RUN_TAG}.log}"

CONTROL_PID=""
WORKER1_PID=""
WORKER2_PID=""

source "$ROOT_DIR/scripts/e2e/lib-migration.sh"
MYSQL57_PORT="$E2E_MYSQL57_PORT"
META_DSN="$(e2e_meta_dsn direct)"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq
e2e_ensure_meta_schema "$ROOT_DIR" "$META_DSN"

kill_pid() {
  local pid="$1"
  if [[ -z "$pid" ]]; then
    return 0
  fi
  kill -CONT "$pid" >/dev/null 2>&1 || true
  kill "$pid" >/dev/null 2>&1 || true
  wait "$pid" >/dev/null 2>&1 || true
}

cleanup() {
  kill_pid "$WORKER2_PID"
  kill_pid "$WORKER1_PID"
  kill_pid "$CONTROL_PID"
}
trap cleanup EXIT

wait_control_plane_ready() {
  for _ in {1..120}; do
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "control-plane not ready in time" >&2
  cat "$CONTROL_LOG" >&2 || true
  return 1
}

start_control_plane() {
  local cp_data_dir="$DATA_DIR/control-plane"
  mkdir -p "$cp_data_dir"

  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="control-plane" \
  BINLOG_SERVER_CLUSTER_STORAGE="per_node" \
  BINLOG_SERVER_LISTEN_ADDR="127.0.0.1:18080" \
  BINLOG_SERVER_DATA_DIR="$cp_data_dir" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$CONTROL_LOG" 2>&1 &
  CONTROL_PID=$!
}

start_worker() {
  local worker_id="$1"
  local health_addr="$2"
  local data_dir="$3"
  local log_file="$4"

  mkdir -p "$data_dir"
  BINLOG_SERVER_MODE="cluster" \
  BINLOG_SERVER_CLUSTER_ROLE="worker" \
  BINLOG_SERVER_CLUSTER_STORAGE="per_node" \
  BINLOG_SERVER_CLUSTER_WORKER_ID="$worker_id" \
  BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR="$health_addr" \
  BINLOG_SERVER_DATA_DIR="$data_dir" \
  BINLOG_SERVER_META_DSN="$META_DSN" \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$log_file" 2>&1 &
  echo "$!"
}

wait_worker_online() {
  local worker_id="$1"
  for _ in {1..90}; do
    local workers
    workers="$(curl -fsS "$API/api/workers?limit=200")"
    if printf '%s' "$workers" | jq -e --arg wid "$worker_id" '.[] | select(.worker_id == $wid and .online == true and (.last_seen_at | tostring | length > 0))' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "worker did not become online in time: worker_id=$worker_id" >&2
  curl -fsS "$API/api/workers?limit=200" >&2 || true
  return 1
}

create_task() {
  local sid
  sid=$((490000 + RANDOM % 10000))

  local resp
  resp="$(curl -fsS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"e2e-sigstop-${RUN_TAG}\",\"cluster_key\":\"e2e-sigstop-${RUN_TAG}\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":${MYSQL57_PORT},\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":${sid}},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")"

  local id
  id="$(printf '%s' "$resp" | jq -r '.id // empty')"
  if [[ -z "$id" || "$id" == "null" ]]; then
    echo "create task failed: $resp" >&2
    exit 1
  fi
  printf '%s' "$id"
}

start_task() {
  local task_id="$1"
  local http_code
  http_code="$(curl -sS -o /tmp/e2e-sigstop-start-${RUN_TAG}.resp -w '%{http_code}' -X POST "$API/api/tasks/$task_id/start")"
  if [[ "$http_code" != "204" ]]; then
    echo "start task failed: http=$http_code body=$(cat /tmp/e2e-sigstop-start-${RUN_TAG}.resp)" >&2
    return 1
  fi
}

stop_task() {
  local task_id="$1"
  local http_code
  http_code="$(curl -sS -o /tmp/e2e-sigstop-stop-${RUN_TAG}.resp -w '%{http_code}' -X POST "$API/api/tasks/$task_id/stop")"
  if [[ "$http_code" != "204" ]]; then
    echo "stop task failed: http=$http_code body=$(cat /tmp/e2e-sigstop-stop-${RUN_TAG}.resp)" >&2
    return 1
  fi
}

task_field() {
  local task_id="$1" field="$2"
  curl -fsS "$API/api/tasks/$task_id" | jq -r ".$field // empty"
}

task_state() { task_field "$1" state; }
task_epoch() { task_field "$1" epoch; }
task_owner() { task_field "$1" owner_worker_id; }
task_desired() { task_field "$1" desired_run; }

dump_threads() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot -N \
    -e "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE COMMAND LIKE 'Binlog Dump%';" 2>/dev/null | tr -d ' \r\n'
}

# The three C3 invariants checked at every sample: never FAILED, desired_run
# never STOP (only an operator writes it — skipped during the intentional
# operator stop at the end), and never more than one dump thread on the source.
assert_invariants() {
  local task_id="$1" where="$2" check_desired="${3:-1}"
  local state desired threads
  state="$(task_state "$task_id")"
  desired="$(task_desired "$task_id")"
  if [[ "$state" == "FAILED" ]]; then
    echo "[$where] task hit FAILED — a node-local fact must not become a cluster terminal state: $(curl -fsS "$API/api/tasks/$task_id")" >&2
    return 1
  fi
  if [[ "$check_desired" == "1" && "$desired" == "STOP" ]]; then
    echo "[$where] desired_run became STOP — only an operator Stop may write it: $(curl -fsS "$API/api/tasks/$task_id")" >&2
    return 1
  fi
  threads="$(dump_threads)"
  if [[ -n "$threads" && "$threads" =~ ^[0-9]+$ && "$threads" -gt 1 ]]; then
    echo "[$where] source has $threads Binlog Dump threads, want at most 1" >&2
    return 1
  fi
  return 0
}

wait_task_state() {
  local task_id="$1" expected="$2" where="${3:-wait}" check_desired="${4:-1}"
  for _ in {1..120}; do
    assert_invariants "$task_id" "$where" "$check_desired" || return 1
    if [[ "$(task_state "$task_id")" == "$expected" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "[$where] task did not become $expected in time: $(curl -fsS "$API/api/tasks/$task_id")" >&2
  return 1
}

write_source_data() {
  local value="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot \
    -e "INSERT INTO binlog_e2e_57.t1(v) VALUES('${value}-1'),('${value}-2'),('${value}-3');"
}

echo "[sigstop] start control-plane + worker1 (per_node, separate data_dirs)"
start_control_plane
wait_control_plane_ready
WORKER1_PID="$(start_worker "$WORKER1_ID" "$WORKER1_HEALTH_ADDR" "$DATA_DIR/worker1" "$WORKER1_LOG")"
wait_worker_online "$WORKER1_ID"
echo "[sigstop] control-plane pid=$CONTROL_PID worker1 pid=$WORKER1_PID"

echo "[sigstop] create + start task on worker1"
TASK_ID="$(create_task)"
start_task "$TASK_ID"
wait_task_state "$TASK_ID" "RUNNING" "initial"
OLD_EPOCH="$(task_epoch "$TASK_ID")"
if [[ ! "$OLD_EPOCH" =~ ^[0-9]+$ || "$OLD_EPOCH" -le 0 ]]; then
  echo "invalid initial epoch: $OLD_EPOCH" >&2
  exit 1
fi
OWNER="$(task_owner "$TASK_ID")"
if [[ "$OWNER" != "$WORKER1_ID" ]]; then
  echo "task owner is $OWNER, want $WORKER1_ID (only worker1 was online)" >&2
  exit 1
fi
echo "[sigstop] task running task_id=$TASK_ID owner=$OWNER epoch=$OLD_EPOCH"

echo "[sigstop] write source data and rotate once so worker1's disk holds the chain"
write_source_data "sigstop-before-rotate-${RUN_TAG}"
docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot -e "FLUSH BINARY LOGS;"
write_source_data "sigstop-after-rotate-${RUN_TAG}"
for _ in {1..30}; do
  [[ -d "$DATA_DIR/worker1/$TASK_ID" ]] && break
  sleep 1
done
[[ -d "$DATA_DIR/worker1/$TASK_ID" ]] || { echo "task dir never appeared on worker1: $DATA_DIR/worker1/$TASK_ID" >&2; exit 1; }

echo "[sigstop] start worker2 so a peer can claim"
WORKER2_PID="$(start_worker "$WORKER2_ID" "$WORKER2_HEALTH_ADDR" "$DATA_DIR/worker2" "$WORKER2_LOG")"
wait_worker_online "$WORKER2_ID"

echo "[sigstop] SIGSTOP worker1 pid=$WORKER1_PID past lease_ttl"
kill -STOP "$WORKER1_PID"

# The peer claims the expired lease, then must yield: the chain tail lives on
# worker1's per_node disk and is not readable on worker2. LEASE_DEGRADED is the
# stable landing of that claim+yield (an epoch bump can flash by between
# samples), so wait for it and keep the largest epoch seen for the log.
echo "[sigstop] wait for the peer claim+yield to LEASE_DEGRADED, asserting C3 invariants"
claim_epoch="$OLD_EPOCH"
degraded=0
for _ in {1..240}; do
  assert_invariants "$TASK_ID" "peer-claim" || exit 1
  cur_epoch="$(task_epoch "$TASK_ID")"
  if [[ "$cur_epoch" =~ ^[0-9]+$ && "$cur_epoch" -gt "$claim_epoch" ]]; then
    claim_epoch="$cur_epoch"
  fi
  if [[ "$(task_state "$TASK_ID")" == "LEASE_DEGRADED" ]]; then
    degraded=1
    break
  fi
  sleep 1
done
if [[ "$degraded" -ne 1 ]]; then
  echo "task never reached LEASE_DEGRADED after SIGSTOP: $(curl -fsS "$API/api/tasks/$TASK_ID")" >&2
  exit 1
fi
echo "[sigstop] peer claimed and yielded (max epoch seen $claim_epoch); LEASE_DEGRADED, no FAILED, desired_run still $(task_desired "$TASK_ID")"

echo "[sigstop] SIGCONT worker1; it must see its stale epoch, yield gracefully, and reclaim larger"
kill -CONT "$WORKER1_PID"

# worker1's tail is readable on its own disk, so it is the node that can run
# the task again — under an epoch larger than the pre-SIGSTOP one (the peer's
# claim and this reclaim both move it forward).
new_epoch=""
for _ in {1..240}; do
  assert_invariants "$TASK_ID" "resume" || exit 1
  cur_state="$(task_state "$TASK_ID")"
  cur_epoch="$(task_epoch "$TASK_ID")"
  cur_owner="$(task_owner "$TASK_ID")"
  if [[ "$cur_state" == "RUNNING" && "$cur_owner" == "$WORKER1_ID" && "$cur_epoch" =~ ^[0-9]+$ && "$cur_epoch" -gt "$OLD_EPOCH" ]]; then
    new_epoch="$cur_epoch"
    break
  fi
  sleep 1
done
if [[ -z "$new_epoch" ]]; then
  echo "worker1 did not resume RUNNING under a larger epoch: $(curl -fsS "$API/api/tasks/$TASK_ID")" >&2
  exit 1
fi
echo "[sigstop] worker1 resumed RUNNING epoch=$new_epoch (> pre-stop $OLD_EPOCH; peer's claim max $claim_epoch)"

if ! kill -0 "$WORKER1_PID" 2>/dev/null; then
  echo "worker1 process died across SIGSTOP/SIGCONT" >&2
  tail -n 50 "$WORKER1_LOG" >&2 || true
  exit 1
fi

echo "[sigstop] write data after resume and check the chain keeps moving"
write_source_data "sigstop-after-resume-${RUN_TAG}"
sleep 5
assert_invariants "$TASK_ID" "post-resume" || exit 1
threads="$(dump_threads)"
if [[ "$threads" != "1" ]]; then
  echo "after resume the source has $threads Binlog Dump threads, want exactly 1" >&2
  exit 1
fi

echo "[sigstop] stop task cleanly (operator Stop writes desired_run=STOP on purpose)"
stop_task "$TASK_ID"
wait_task_state "$TASK_ID" "STOPPED" "stop" 0

echo "[sigstop] success: per_node SIGSTOP takeover never wrote FAILED/STOP; original owner resumed under epoch $new_epoch; source never held more than one dump"
