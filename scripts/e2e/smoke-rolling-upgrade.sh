#!/usr/bin/env bash
# input: canonical E2E database topology (compose mysql57 as source,
#   meta-primary as meta), a tag old enough to predate migration 000011
#   (E2E_OLD_REF, default v0.5.83), one meta DSN
# output: ADR 0011 §4.2 row 12 rolling-upgrade fence e2e — proves the four
#   ordered gates: live registration refuses migrate up; expired registration
#   lets it through; a post-migration protocol-0 registration opens
#   CRITICAL MIXED_VERSION on _cluster and blocks new claims while the old
#   node keeps dumping; the old node leaving closes the finding, the
#   foreign per_node tail degrades the task until the documented operator
#   copy + Start path restores it; live protocol-11 registrations refuse
#   migrate down and the downed schema runs the old binary again
# pos: integration-test automation layer validating end-to-end cluster
#   upgrade/downgrade fencing
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-rolling-upgrade-$(date +%s)}"
# Builds cd into the old worktree: every artifact path must be absolute.
mkdir -p "$DATA_DIR"
DATA_DIR="$(cd "$DATA_DIR" && pwd)"
OLD_REF="${E2E_OLD_REF:-v0.5.83}"
OLD_WORKER_ID="${E2E_OLD_WORKER_ID:-e2e-ru-old-1}"
NEW_WORKER_ID="${E2E_NEW_WORKER_ID:-e2e-ru-new-2}"
OLD_HEALTH_ADDR="${E2E_OLD_HEALTH_ADDR:-127.0.0.1:18081}"
NEW_HEALTH_ADDR="${E2E_NEW_HEALTH_ADDR:-127.0.0.1:18082}"

RUN_TAG="$(date +%s)"
OLD_TREE="${E2E_OLD_TREE:-$DATA_DIR/oldtree}"
OLD_BIN="${E2E_OLD_BIN:-$DATA_DIR/bin/binlog-server-old}"
OLD_MIGRATE="${E2E_OLD_MIGRATE:-$DATA_DIR/bin/migrate-old}"
NEW_BIN="${E2E_NEW_BIN:-$DATA_DIR/bin/binlog-server-new}"
NEW_MIGRATE="${E2E_NEW_MIGRATE:-$DATA_DIR/bin/migrate-new}"

CONTROL_LOG="${E2E_CONTROL_LOG:-/tmp/binlog-server-e2e-ru-control-${RUN_TAG}.log}"
OLD_WORKER_LOG="${E2E_OLD_WORKER_LOG:-/tmp/binlog-server-e2e-ru-oldworker-${RUN_TAG}.log}"
NEW_WORKER_LOG="${E2E_NEW_WORKER_LOG:-/tmp/binlog-server-e2e-ru-newworker-${RUN_TAG}.log}"

CONTROL_PID=""
OLD_WORKER_PID=""
NEW_WORKER_PID=""

source "$ROOT_DIR/scripts/e2e/lib-migration.sh"
MYSQL57_PORT="$E2E_MYSQL57_PORT"
META_DSN="${BINLOG_SERVER_META_DSN:-$(e2e_meta_dsn direct)}"
CONFIG_PATH="$ROOT_DIR/deploy/e2e/config.yaml"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd git
need_cmd go
need_cmd jq

meta_sql() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary mysql -umeta -pmetapass -N binlog_meta -e "$1" 2>/dev/null
}

schema_version() {
  meta_sql "SELECT version FROM schema_migrations" | tr -d ' \r\n'
}

dump_threads() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot -N \
    -e "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE COMMAND LIKE 'Binlog Dump%';" 2>/dev/null | tr -d ' \r\n'
}

source_sql() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot -N -e "$1" 2>/dev/null
}

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
  kill_pid "$NEW_WORKER_PID"
  kill_pid "$OLD_WORKER_PID"
  kill_pid "$CONTROL_PID"
  git -C "$ROOT_DIR" worktree remove --force "$OLD_TREE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

migrate_new() {
  META_DSN="$META_DSN" "$NEW_MIGRATE" "$@"
}

migrate_old() {
  META_DSN="$META_DSN" "$OLD_MIGRATE" "$@"
}

# start_proc <binary> <role> <worker_id|-> <health_addr|-> <data_dir> <log>
start_proc() {
  local bin="$1" role="$2" worker_id="$3" health_addr="$4" data_dir="$5" log="$6"
  mkdir -p "$data_dir"
  local -a env_prefix=(
    "BINLOG_SERVER_MODE=cluster"
    "BINLOG_SERVER_CLUSTER_ROLE=$role"
    "BINLOG_SERVER_CLUSTER_STORAGE=per_node"
    "BINLOG_SERVER_DATA_DIR=$data_dir"
    "BINLOG_SERVER_META_DSN=$META_DSN"
    "BINLOG_SERVER_CLUSTER_LEASE_TTL_SEC=15"
    "BINLOG_SERVER_HEALTH_SWEEP_INTERVAL_SEC=2"
    "BINLOG_SERVER_HEALTH_NODE_GONE_AFTER_SEC=45"
  )
  if [[ "$role" == "control-plane" ]]; then
    env_prefix+=("BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18080")
  else
    env_prefix+=("BINLOG_SERVER_CLUSTER_WORKER_ID=$worker_id")
    env_prefix+=("BINLOG_SERVER_CLUSTER_WORKER_HEALTH_LISTEN_ADDR=$health_addr")
  fi
  env "${env_prefix[@]}" nohup "$bin" --config "$CONFIG_PATH" >"$log" 2>&1 &
  echo "$!"
}

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

http_code() {
  local method="$1" path="$2"
  curl -sS -o "/tmp/e2e-ru-${RUN_TAG}.resp" -w '%{http_code}' -X "$method" "$API$path"
}

task_field() { curl -fsS "$API/api/tasks/$1" | jq -r ".$2 // empty"; }
task_state() { task_field "$1" state; }
task_owner() { task_field "$1" owner_worker_id; }

create_task() {
  local name="$1" sid resp id
  sid=$((490000 + RANDOM % 10000))
  resp="$(curl -fsS -X POST "$API/api/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"cluster_key\":\"$name\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":${MYSQL57_PORT},\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":${sid}},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}")"
  id="$(printf '%s' "$resp" | jq -r '.id // empty')"
  [[ -n "$id" && "$id" != "null" ]] || { echo "create task failed: $resp" >&2; return 1; }
  printf '%s' "$id"
}

wait_running_owner() {
  local task_id="$1" owner="$2" secs="${3:-120}"
  local i
  for i in $(seq 1 "$secs"); do
    if [[ "$(task_state "$task_id")" == "RUNNING" && "$(task_owner "$task_id")" == "$owner" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "task $task_id not RUNNING under $owner: $(curl -fsS "$API/api/tasks/$task_id")" >&2
  return 1
}

open_crit() {
  local task_id="$1" kind="$2"
  curl -fsS "$API/api/findings?limit=500" | jq -e --arg t "$task_id" --arg k "$kind" '.[] | select(.task_id == $t and .kind == $k and .severity == "CRITICAL" and (.resolved_at == null))' >/dev/null
}

wait_open_crit() {
  local task_id="$1" kind="$2" secs="${3:-60}"
  local i
  for i in $(seq 1 "$secs"); do
    if open_crit "$task_id" "$kind"; then
      return 0
    fi
    sleep 1
  done
  echo "finding $kind on $task_id never opened" >&2
  return 1
}

wait_crit_closed() {
  local task_id="$1" kind="$2" secs="${3:-60}"
  local i
  for i in $(seq 1 "$secs"); do
    if ! open_crit "$task_id" "$kind"; then
      return 0
    fi
    sleep 1
  done
  echo "finding $kind on $task_id never closed" >&2
  return 1
}

# Unexpired registration rows (optionally filtered to a protocol ceiling).
live_registrations() {
  local where="${1:-}"
  meta_sql "SELECT COUNT(*) FROM worker_registrations WHERE lease_expire_at > NOW(6) $where" | tr -d ' \r\n'
}

wait_no_live_registrations() {
  local secs="${1:-90}"
  local i
  for i in $(seq 1 "$secs"); do
    if [[ "$(live_registrations)" == "0" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "worker_registrations still live after ${secs}s:" >&2
  meta_sql "SELECT worker_id, cluster_protocol_version, lease_expire_at > NOW(6) FROM worker_registrations" >&2 || true
  return 1
}

wait_live_registration() {
  local worker_id="$1" secs="${2:-60}"
  local i
  for i in $(seq 1 "$secs"); do
    if [[ "$(meta_sql "SELECT COUNT(*) FROM worker_registrations WHERE worker_id='$worker_id' AND lease_expire_at > NOW(6)" | tr -d ' \r\n')" == "1" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "registration for $worker_id never went live" >&2
  return 1
}

echo "== build old binaries ($OLD_REF) =="
mkdir -p "$(dirname "$OLD_BIN")"
git -C "$ROOT_DIR" rev-parse -q --verify "refs/tags/$OLD_REF" >/dev/null   || git -C "$ROOT_DIR" fetch origin "refs/tags/$OLD_REF:refs/tags/$OLD_REF"
git -C "$ROOT_DIR" worktree add --detach "$OLD_TREE" "$OLD_REF" >/dev/null
(
  cd "$OLD_TREE"
  CGO_ENABLED=0 go build -o "$OLD_BIN" ./cmd/binlog-server
  CGO_ENABLED=0 go build -o "$OLD_MIGRATE" ./cmd/migrate
)

echo "== build current binaries =="
(
  cd "$ROOT_DIR"
  CGO_ENABLED=0 go build -o "$NEW_BIN" ./cmd/binlog-server
  CGO_ENABLED=0 go build -o "$NEW_MIGRATE" ./cmd/migrate
)

echo "== land meta at the old schema level =="
e2e_ensure_meta_schema "$ROOT_DIR" "$META_DSN"
# The suite guarantees schema HEAD (11). The old binary predates 000011, so
# roll one step back before old processes start. No live registrations yet,
# so the 000011 down precheck passes.
migrate_new down --steps 1 --allow-destructive >/dev/null
[[ "$(schema_version)" == "10" ]] || { echo "schema at $(schema_version), want 10" >&2; exit 1; }

echo "== old world: old control-plane + old worker =="
CONTROL_PID="$(start_proc "$OLD_BIN" control-plane - - "$DATA_DIR/control-plane" "$CONTROL_LOG")"
wait_control_plane_ready
OLD_WORKER_PID="$(start_proc "$OLD_BIN" worker "$OLD_WORKER_ID" "$OLD_HEALTH_ADDR" "$DATA_DIR/w-old" "$OLD_WORKER_LOG")"
wait_live_registration "$OLD_WORKER_ID"

TASK_ID="$(create_task "e2e-ru-${RUN_TAG}")"
[[ "$(http_code POST "/api/tasks/$TASK_ID/start")" == "204" ]] || { echo "start failed: $(cat "/tmp/e2e-ru-${RUN_TAG}.resp")" >&2; exit 1; }
wait_running_owner "$TASK_ID" "$OLD_WORKER_ID"
[[ "$(dump_threads)" == "1" ]] || { echo "[ru] dump threads=$(dump_threads), want 1" >&2; exit 1; }
echo "[ru:oldworld] task RUNNING on $OLD_WORKER_ID at schema 10"

echo "== fence 1: live registration refuses migrate up =="
if migrate_new up >/tmp/e2e-ru-${RUN_TAG}.up1.log 2>&1; then
  echo "migrate up unexpectedly succeeded while $OLD_WORKER_ID registration was live" >&2
  exit 1
fi
grep -qi "Division by 0" /tmp/e2e-ru-${RUN_TAG}.up1.log || { echo "note: refused-up log did not show the precheck divisor (dumping):"; cat /tmp/e2e-ru-${RUN_TAG}.up1.log; }
[[ "$(schema_version)" == "10" ]] || { echo "schema moved to $(schema_version) despite refused up" >&2; exit 1; }
echo "[ru:fence1] migrate up refused under live registration, schema stayed 10"

echo "== freeze old worker past its registration TTL =="
kill -STOP "$OLD_WORKER_PID"
wait_no_live_registrations 90

echo "== migrate up now lands =="
migrate_new up
[[ "$(schema_version)" == "11" ]] || { echo "schema at $(schema_version), want 11" >&2; exit 1; }
echo "[ru:up] schema at 11"

echo "== old worker returns, re-registers at protocol 0 =="
kill -CONT "$OLD_WORKER_PID"
# Re-registration only lands once the expired row lets AcquireWorkerRegistration win.
for _ in $(seq 1 90); do
  if [[ "$(live_registrations "AND cluster_protocol_version = 0")" == "1" ]]; then
    break
  fi
  sleep 1
done
[[ "$(live_registrations "AND cluster_protocol_version = 0")" == "1" ]] || { echo "no live protocol-0 registration after old worker resumed" >&2; exit 1; }
# Old binary keeps working against migrated meta: renewals, catalog rows and
# the dump itself are unfenced by design (ADR 0011 §4.2 row 12).
wait_running_owner "$TASK_ID" "$OLD_WORKER_ID"
for _ in $(seq 1 60); do
  [[ "$(dump_threads)" == "1" ]] && break
  sleep 1
done
[[ "$(dump_threads)" == "1" ]] || { echo "old worker did not resume its dump" >&2; exit 1; }
echo "[ru:mixed] protocol-0 registration live, old worker still dumps"

echo "== new nodes: MIXED_VERSION opens, claims are fenced =="
# The new control-plane binds the same API port: stop the old cp first.
kill_pid "$CONTROL_PID"
CONTROL_PID="$(start_proc "$NEW_BIN" control-plane - - "$DATA_DIR/control-plane-new" "$CONTROL_LOG.new")"
wait_control_plane_ready
NEW_WORKER_PID="$(start_proc "$NEW_BIN" worker "$NEW_WORKER_ID" "$NEW_HEALTH_ADDR" "$DATA_DIR/w-new" "$NEW_WORKER_LOG")"
wait_live_registration "$NEW_WORKER_ID"
wait_open_crit "_cluster" "MIXED_VERSION" 90
[[ "$(curl -sS -o /dev/null -w '%{http_code}' "$API/api/health")" == "503" ]] || { echo "/api/health not 503 while MIXED_VERSION open" >&2; exit 1; }
# Claim fence: while the protocol-0 registration is live, the task's owner
# must stay the old worker — give the new node a window it would claim in.
sleep 20
[[ "$(task_owner "$TASK_ID")" == "$OLD_WORKER_ID" ]] || { echo "task owner moved to $(task_owner "$TASK_ID") under MIXED_VERSION" >&2; exit 1; }
echo "[ru:mixed] MIXED_VERSION open on _cluster, /api/health 503, no takeover"

echo "== old worker leaves: gate clears, tail is foreign on per_node =="
# Land a data event in the open segment before the old worker leaves. The
# operator copy below only adopts when binlog.DurableResume finds a complete
# event (end log_pos > 0) in the copied .open file; a preamble-only copy
# still resolves Missing through the catalog file_path (#581 family, a real
# gap — an empty tail cannot be adopted even after the operator copies it).
source_sql "CREATE DATABASE IF NOT EXISTS e2e_ru;
  CREATE TABLE IF NOT EXISTS e2e_ru.handover (id INT PRIMARY KEY AUTO_INCREMENT);
  INSERT INTO e2e_ru.handover VALUES (NULL);"
sleep 3
kill_pid "$OLD_WORKER_PID"; OLD_WORKER_PID=""
wait_crit_closed "_cluster" "MIXED_VERSION" 120
# ADR 0011: a per_node claim meeting the previous owner's foreign .open tail
# is TailUnreadable — the new worker releases the epoch, writes an
# open-ended refusal (refuse_until NULL) and the task waits in
# LEASE_DEGRADED, desired RUN, with /api/health 503. Automatic takeover to
# RUNNING is not possible: no surviving node has the old owner's tail.
for _ in $(seq 1 180); do
  [[ "$(task_state "$TASK_ID")" == "LEASE_DEGRADED" ]] && break
  sleep 1
done
[[ "$(task_state "$TASK_ID")" == "LEASE_DEGRADED" ]] || { echo "task state=$(task_state "$TASK_ID"), want LEASE_DEGRADED after foreign-tail claim" >&2; exit 1; }
[[ "$(task_field "$TASK_ID" desired_run)" == "RUN" ]] || { echo "desired_run flipped: $(task_field "$TASK_ID" desired_run)" >&2; exit 1; }
# ADR 0011 rule 7 expects a wait nobody can end to surface as CRITICAL +
# /api/health 503. Poll for the open finding, then grade honestly: CRITICAL
# must show 503; anything weaker is the #580-family gap — recorded, not
# masked, because it is tangential to the migration fence under test.
tail_sev=""
for _ in $(seq 1 90); do
  tail_sev="$(curl -fsS "$API/api/findings?limit=500" | jq -r --arg t "$TASK_ID" '[.[] | select(.task_id == $t and (.resolved_at == null)) | .severity] | first // empty')"
  [[ -n "$tail_sev" ]] && break
  sleep 1
done
if [[ "$tail_sev" == "CRITICAL" ]]; then
  [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$API/api/health")" == "503" ]] || { echo "/api/health not 503 while the tail finding is CRITICAL" >&2; exit 1; }
  echo "[ru:handover] LEASE_DEGRADED on foreign tail, CRITICAL finding, health 503"
else
  echo "[ru:handover] LEASE_DEGRADED on foreign tail; GAP (#580 family): open finding severity='${tail_sev:-none}', ADR expects CRITICAL+503 while a tail waits unreadable"
fi

echo "== operator path: copy the tail onto the new worker, then Start =="
cp -a "$DATA_DIR/w-old/$TASK_ID" "$DATA_DIR/w-new/$TASK_ID"
[[ "$(http_code POST "/api/tasks/$TASK_ID/start")" == "204" ]] || { echo "operator start failed: $(cat "/tmp/e2e-ru-${RUN_TAG}.resp")" >&2; exit 1; }
wait_running_owner "$TASK_ID" "$NEW_WORKER_ID" 180
[[ "$(dump_threads)" == "1" ]] || { echo "[ru] dump threads=$(dump_threads), want 1" >&2; exit 1; }
echo "[ru:handover] task RUNNING on $NEW_WORKER_ID after operator copy + Start"

echo "== fence 2: live protocol-11 registration refuses migrate down =="
if migrate_new down --steps 1 --allow-destructive >/tmp/e2e-ru-${RUN_TAG}.down1.log 2>&1; then
  echo "migrate down unexpectedly succeeded while a protocol-11 registration was live" >&2
  exit 1
fi
[[ "$(schema_version)" == "11" ]] || { echo "schema moved to $(schema_version) despite refused down" >&2; exit 1; }
echo "[ru:fence2] migrate down refused under live protocol-11 registration"

echo "== stop new fleet, migrate down lands =="
kill_pid "$NEW_WORKER_PID"; NEW_WORKER_PID=""
wait_no_live_registrations 90
migrate_new down --steps 1 --allow-destructive >/dev/null
[[ "$(schema_version)" == "10" ]] || { echo "schema at $(schema_version), want 10" >&2; exit 1; }
[[ -z "$(meta_sql "SELECT column_name FROM information_schema.columns WHERE table_schema='binlog_meta' AND table_name='backup_tasks' AND column_name='row_generation'")" ]] \
  || { echo "row_generation still present after down" >&2; exit 1; }
echo "[ru:down] schema back at 10, 000011 columns dropped"

echo "== round-trip: old binary runs the downgraded schema =="
kill_pid "$CONTROL_PID"
CONTROL_PID="$(start_proc "$OLD_BIN" control-plane - - "$DATA_DIR/control-plane-old" "$CONTROL_LOG.old")"
wait_control_plane_ready
OLD_WORKER_PID="$(start_proc "$OLD_BIN" worker "$OLD_WORKER_ID" "$OLD_HEALTH_ADDR" "$DATA_DIR/w-old" "$OLD_WORKER_LOG")"
wait_live_registration "$OLD_WORKER_ID"
wait_running_owner "$TASK_ID" "$OLD_WORKER_ID"
[[ "$(dump_threads)" == "1" ]] || { echo "[ru] dump threads=$(dump_threads), want 1" >&2; exit 1; }
echo "[ru:roundtrip] old binary claimed the task on the downgraded schema"

echo "== final teardown: stop task =="
[[ "$(http_code POST "/api/tasks/$TASK_ID/stop")" == "204" ]] || { echo "final stop failed: $(cat "/tmp/e2e-ru-${RUN_TAG}.resp")" >&2; exit 1; }
for _ in $(seq 1 120); do
  case "$(task_state "$TASK_ID")" in
    STOPPED|FAILED) break ;;
  esac
  sleep 1
done
[[ "$(task_state "$TASK_ID")" == "STOPPED" ]] || { echo "task ended as $(task_state "$TASK_ID"), want STOPPED" >&2; exit 1; }
for _ in $(seq 1 60); do
  [[ "$(dump_threads)" == "0" ]] && break
  sleep 1
done
[[ "$(dump_threads)" == "0" ]] || { echo "residual dump threads: $(dump_threads)" >&2; exit 1; }

echo "rolling-upgrade fence e2e passed"
