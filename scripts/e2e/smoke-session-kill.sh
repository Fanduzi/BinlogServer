#!/usr/bin/env bash
# input: docker mysql:8.0 (compose mysql80) and mariadb:11.4, the suite metadata DSN, curl and jq
# output: SIGSTOP of a holder opened through OpenClusterMeta is released inside the session-kill bound on MySQL 8.0 and MariaDB; TestSessionKillDropsSleepingForUpdate, TestQA484DeadlineMargin and TestQA484PoolPressure pass on both; a database-only account's live evaluator opens META_LOCK_STALLED and /api/health returns 503; a cluster server with one task and the default 60s sweep keeps /api/health at 200 with no HEALTH_STALE
# pos: ADR 0011 PR-2 e2e cells, in the quick profile so unit-and-e2e runs them
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
e2e_topology_init

RUN_TAG="$(date +%s)"
WORK="${E2E_SESSION_KILL_DIR:-$ROOT_DIR/tmp/e2e/session-kill-${RUN_TAG}}"
BIN="$WORK/binlog-server"
HOLD="$WORK/hold"
META_DSN="${BINLOG_SERVER_META_DSN:-$(e2e_meta_dsn direct)}"
API="http://127.0.0.1:18120"
NP_API="http://127.0.0.1:18121"
HEALTH_PID=""
HOLDER_PID=""
NP_PID=""
NP_HOLD=""
SCRATCH_DB=""
MARIA_NAME="binlog-e2e-session-kill-maria"
MARIA_PORT="${E2E_SESSION_KILL_MARIA_PORT:-13348}"
MYSQL80_NAME="${E2E_MYSQL80_CONTAINER:-binlog-e2e-mysql80}"

need_cmd() { command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }; }
need_cmd docker; need_cmd curl; need_cmd jq; need_cmd go; need_cmd ps

log() { echo "[session-kill] $*"; }

cleanup() {
  local rc=$?
  if [[ "$rc" -ne 0 ]]; then
    [[ -f "$WORK/health-server.log" ]] && { echo "== health server" >&2; tail -n 80 "$WORK/health-server.log" >&2 || true; }
  fi
  if [[ -n "$HOLDER_PID" ]]; then
    kill -CONT "$HOLDER_PID" >/dev/null 2>&1 || true
    kill "$HOLDER_PID" >/dev/null 2>&1 || true
    wait "$HOLDER_PID" >/dev/null 2>&1 || true
  fi
  if [[ -n "$HEALTH_PID" ]]; then
    kill "$HEALTH_PID" >/dev/null 2>&1 || true
    wait "$HEALTH_PID" >/dev/null 2>&1 || true
  fi
  if [[ -n "$NP_HOLD" ]]; then
    kill -CONT "$NP_HOLD" >/dev/null 2>&1 || true
    kill "$NP_HOLD" >/dev/null 2>&1 || true
    wait "$NP_HOLD" >/dev/null 2>&1 || true
  fi
  if [[ -n "$NP_PID" ]]; then
    kill "$NP_PID" >/dev/null 2>&1 || true
    wait "$NP_PID" >/dev/null 2>&1 || true
  fi
  docker rm -f -v "$MARIA_NAME" >/dev/null 2>&1 || true
  if [[ -n "$SCRATCH_DB" ]]; then
    docker exec -i binlog-e2e-meta-primary mysql -uroot -proot -e "DROP DATABASE IF EXISTS ${SCRATCH_DB};" >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap cleanup EXIT

mkdir -p "$WORK"
( cd "$ROOT_DIR" && CGO_ENABLED=0 go build -o "$BIN" ./cmd/binlog-server )
( cd "$ROOT_DIR" && CGO_ENABLED=0 go build -o "$HOLD" ./scripts/e2e/hold_for_update.go )

mysql80_sql() {
  docker exec -i "$MYSQL80_NAME" mysql -uroot -proot -Nse "$1"
}

maria_sql() {
  docker exec -i "$MARIA_NAME" mariadb -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "$1" 2>/dev/null
}

create_db() {
  local sql_fn="$1" db="$2"
  "$sql_fn" "DROP DATABASE IF EXISTS ${db}; CREATE DATABASE ${db};"
}

migrate_db() {
  local dsn="$1"
  ( cd "$ROOT_DIR" && META_DSN="$dsn" go run ./cmd/migrate up )
}

insert_lease() {
  local sql_fn="$1" db="$2"
  "$sql_fn" "INSERT INTO ${db}.task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
    VALUES ('t-stop', 'w1', 1, DATE_ADD(NOW(6), INTERVAL 1 HOUR), NOW(6))"
}

# run_required_test fails when the test skips or does not pass. go test
# exits 0 on a skip, so the log is checked as well.
run_required_test() {
  local name="$1"
  local logf="$2"
  shift 2
  set +e
  ( cd "$ROOT_DIR" && go test -count=1 -timeout 240s -v -run "^${name}$" "$@" ) >"$logf" 2>&1
  local rc=$?
  set -e
  if grep -E -q -- '--- SKIP:' "$logf"; then
    echo "$name skipped" >&2
    cat "$logf" >&2
    return 1
  fi
  if [[ "$rc" -ne 0 ]]; then
    echo "$name failed" >&2
    cat "$logf" >&2
    return 1
  fi
  if ! grep -q -- "--- PASS: ${name}" "$logf"; then
    echo "$name did not pass" >&2
    cat "$logf" >&2
    return 1
  fi
  log "$name passed"
}

stop_holder() {
  local dsn="$1" peer="$2" label="$3"
  local logf="$WORK/${label}.hold"
  "$HOLD" "$dsn" t-stop >"$logf" 2>&1 &
  HOLDER_PID=$!
  local i
  for i in $(seq 1 100); do
    if grep -q HOLDING "$logf"; then
      break
    fi
    if ! kill -0 "$HOLDER_PID" 2>/dev/null; then
      echo "$label holder exited before HOLDING" >&2
      cat "$logf" >&2 || true
      return 1
    fi
    if [[ "$i" -eq 100 ]]; then
      echo "$label holder did not lock the row" >&2
      cat "$logf" >&2 || true
      return 1
    fi
    sleep 0.1
  done
  if ! kill -0 "$HOLDER_PID" 2>/dev/null; then
    echo "$label holder exited after HOLDING" >&2
    cat "$logf" >&2 || true
    return 1
  fi
  if ! kill -STOP "$HOLDER_PID" 2>/dev/null; then
    echo "$label holder was gone before SIGSTOP" >&2
    cat "$logf" >&2 || true
    return 1
  fi
  local stat
  stat="$(ps -o stat= -p "$HOLDER_PID" | tr -d '[:space:]')"
  case "$stat" in
    *T*) ;;
    *) echo "$label process not stopped (stat=$stat)" >&2; cat "$logf" >&2 || true; return 1 ;;
  esac
  # The stopped client must still hold the row. A process that exited
  # would have released it already, which is not the server timeout.
  if "$HOLD" --take --timeout 400ms "$peer" t-stop; then
    echo "$label lock was already free after SIGSTOP" >&2
    cat "$logf" >&2 || true
    return 1
  fi
  if ! "$HOLD" --take --timeout 8s "$peer" t-stop; then
    echo "$label peer did not take the row after SIGSTOP" >&2
    cat "$logf" >&2 || true
    return 1
  fi
  kill -CONT "$HOLDER_PID" >/dev/null 2>&1 || true
  kill "$HOLDER_PID" >/dev/null 2>&1 || true
  wait "$HOLDER_PID" >/dev/null 2>&1 || true
  HOLDER_PID=""
  log "$label SIGSTOP released the FOR UPDATE inside the bound"
}

cell_mysql80() {
  local db="binlog_sk_${RUN_TAG}"
  local i
  for i in $(seq 1 60); do
    if mysql80_sql "SELECT VERSION()" >/dev/null 2>&1; then
      break
    fi
    if [[ "$i" -eq 60 ]]; then
      echo "mysql80 is not ready" >&2
      return 1
    fi
    sleep 1
  done
  local version
  version="$(mysql80_sql "SELECT VERSION()")"
  [[ "$version" == 8.0* ]] || { echo "want MySQL 8.0, got $version" >&2; return 1; }
  create_db mysql80_sql "$db"
  local dsn="root:root@tcp(127.0.0.1:${E2E_MYSQL80_PORT})/${db}?parseTime=true"
  migrate_db "$dsn"
  insert_lease mysql80_sql "$db"
  local peer="root:root@tcp(127.0.0.1:${E2E_MYSQL80_PORT})/${db}?parseTime=true&innodb_lock_wait_timeout=8"
  stop_holder "$dsn" "$peer" mysql80
  mysql80_sql "DROP DATABASE IF EXISTS ${db}" || true
}

start_mariadb() {
  docker rm -f -v "$MARIA_NAME" >/dev/null 2>&1 || true
  docker run -d --name "$MARIA_NAME" -p "127.0.0.1:${MARIA_PORT}:3306" \
    -e MARIADB_ROOT_PASSWORD=root -e MARIADB_ROOT_HOST=% \
    mariadb:11.4 >/dev/null
  local i
  for i in $(seq 1 120); do
    if maria_sql "SELECT VERSION()" >/dev/null 2>&1; then
      break
    fi
    if [[ "$i" -eq 120 ]]; then
      echo "mariadb not ready" >&2
      docker logs "$MARIA_NAME" >&2 || true
      return 1
    fi
    sleep 1
  done
  local version
  version="$(maria_sql "SELECT VERSION()")"
  [[ "$version" == *MariaDB* ]] || { echo "want MariaDB, got $version" >&2; return 1; }
}

cell_mariadb() {
  local db="binlog_sk_${RUN_TAG}"
  create_db maria_sql "$db"
  local dsn="root:root@tcp(127.0.0.1:${MARIA_PORT})/${db}?parseTime=true"
  migrate_db "$dsn"
  insert_lease maria_sql "$db"
  local peer="root:root@tcp(127.0.0.1:${MARIA_PORT})/${db}?parseTime=true&innodb_lock_wait_timeout=8"
  stop_holder "$dsn" "$peer" mariadb
}

cell_pool_test() {
  local mysql_root="root:root@tcp(127.0.0.1:${E2E_MYSQL80_PORT})/"
  local maria_root="root:root@tcp(127.0.0.1:${MARIA_PORT})/"
  BINLOG_TEST_MYSQL_ROOT_DSNS="${mysql_root},${maria_root}" \
    run_required_test TestSessionKillDropsSleepingForUpdate "$WORK/TestSessionKillDropsSleepingForUpdate.log" ./internal/meta
  local n
  n="$(grep -c -- '--- PASS: TestSessionKillDropsSleepingForUpdate/server-' "$WORK/TestSessionKillDropsSleepingForUpdate.log" || true)"
  if [[ "$n" -ne 2 ]]; then
    echo "TestSessionKillDropsSleepingForUpdate passed on ${n} servers, want 2" >&2
    return 1
  fi
  log "pool test passed on MySQL 8.0 and MariaDB"
}

stop_np() {
  if [[ -n "$NP_HOLD" ]]; then
    kill -CONT "$NP_HOLD" >/dev/null 2>&1 || true
    kill "$NP_HOLD" >/dev/null 2>&1 || true
    wait "$NP_HOLD" >/dev/null 2>&1 || true
    NP_HOLD=""
  fi
  if [[ -n "$NP_PID" ]]; then
    kill "$NP_PID" >/dev/null 2>&1 || true
    wait "$NP_PID" >/dev/null 2>&1 || true
    NP_PID=""
  fi
}

cell_noprocess() {
  local sql_fn="$1" port="$2" label="$3"
  local db="binlog_np_${RUN_TAG}"
  stop_np
  create_db "$sql_fn" "$db"
  local admin="root:root@tcp(127.0.0.1:${port})/${db}?parseTime=true"
  migrate_db "$admin"
  "$sql_fn" "CREATE USER IF NOT EXISTS 'sknp'@'%' IDENTIFIED BY 'sknp'"
  "$sql_fn" "GRANT ALL PRIVILEGES ON ${db}.* TO 'sknp'@'%'"
  "$sql_fn" "FLUSH PRIVILEGES"
  local grants
  grants="$("$sql_fn" "SHOW GRANTS FOR 'sknp'@'%'")"
  if printf '%s\n' "$grants" | grep -Eiq 'PROCESS|SUPER|ALL PRIVILEGES ON \*\.\*'; then
    echo "$label account is not limited to ${db}.*: $grants" >&2
    return 1
  fi
  local dsn="sknp:sknp@tcp(127.0.0.1:${port})/${db}?parseTime=true"
  BINLOG_TEST_NOPROCESS_DSN="$dsn" \
    run_required_test TestMetaLockStallWithoutProcess "$WORK/noprocess-${label}.log" ./internal/meta
  "$sql_fn" "INSERT INTO ${db}.backup_tasks (id, name, cluster_key, state, last_error, owner_worker_id, epoch, run_id, source_json, start_json, storage_json, updated_at, desired_run, spec_revision, applied_spec_revision, failed_spec_revision, retry_attempt, consecutive_source_failures) VALUES ('sk-np', 'sk-np', 'sk-np-${label}-${RUN_TAG}', 'STOPPED', '', 'sk-np', 1, '', '{}', '{}', '{}', UTC_TIMESTAMP(6), 'STOP', 1, 1, 0, 0, 0)"
  "$sql_fn" "INSERT INTO ${db}.task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at) VALUES ('sk-np', 'sk-np', 1, DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 1 HOUR), UTC_TIMESTAMP(6))"
  local holdlog="$WORK/noprocess-${label}.hold"
  "$HOLD" --plain "$admin" sk-np >"$holdlog" 2>&1 &
  NP_HOLD=$!
  local i
  for i in $(seq 1 100); do
    if grep -q HOLDING "$holdlog"; then
      break
    fi
    if ! kill -0 "$NP_HOLD" 2>/dev/null; then
      echo "$label plain holder exited" >&2
      cat "$holdlog" >&2 || true
      return 1
    fi
    if [[ "$i" -eq 100 ]]; then
      echo "$label plain holder did not lock" >&2
      cat "$holdlog" >&2 || true
      return 1
    fi
    sleep 0.1
  done
  kill -STOP "$NP_HOLD"
  local data="$WORK/noprocess-${label}-data"
  mkdir -p "$data"
  local srvlog="$WORK/noprocess-${label}-server.log"
  (
    cd "$ROOT_DIR"
    exec env \
      BINLOG_SERVER_MODE=cluster \
      BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18121 \
      BINLOG_SERVER_DATA_DIR="$data" \
      BINLOG_SERVER_META_DSN="$dsn" \
      BINLOG_SERVER_CLUSTER_WORKER_ID=sk-np \
      BINLOG_SERVER_HEALTH_SWEEP_INTERVAL_SEC=2 \
      "$BIN" --config "$ROOT_DIR/deploy/e2e/config.yaml"
  ) >"$srvlog" 2>&1 &
  NP_PID=$!
  local saw=0
  for i in $(seq 1 90); do
    if ! kill -0 "$NP_PID" 2>/dev/null; then
      echo "$label evaluator exited" >&2
      tail -n 80 "$srvlog" >&2 || true
      return 1
    fi
    local body code status
    body="$(curl -sS -m 5 -w '\n%{http_code}' "$NP_API/api/health" || true)"
    code="$(printf '%s' "$body" | tail -n 1)"
    body="$(printf '%s' "$body" | sed '$d')"
    if [[ "$code" == "503" ]] && printf '%s' "$body" | grep -q META_LOCK_STALLED; then
      status="$(printf '%s' "$body" | jq -r '.status')"
      if [[ "$status" != "degraded" ]]; then
        echo "$label /api/health status=$status, want degraded: $body" >&2
        return 1
      fi
      saw=1
      break
    fi
    sleep 0.5
  done
  if [[ "$saw" -ne 1 ]]; then
    echo "$label live evaluator did not open META_LOCK_STALLED" >&2
    curl -sS -m 5 -w '\n%{http_code}\n' "$NP_API/api/health" >&2 || true
    tail -n 80 "$srvlog" >&2 || true
    return 1
  fi
  stop_np
  "$sql_fn" "DROP USER IF EXISTS 'sknp'@'%'" || true
  "$sql_fn" "DROP DATABASE IF EXISTS ${db}" || true
  log "$label META_LOCK_STALLED opened through the live evaluator without PROCESS"
}

cell_qa_margin() {
  local mysql_root="root:root@tcp(127.0.0.1:${E2E_MYSQL80_PORT})/"
  local maria_root="root:root@tcp(127.0.0.1:${MARIA_PORT})/"
  local dsns="${mysql_root},${maria_root}"
  QA_PRE_HOLD_MS=1500 BINLOG_TEST_MYSQL_ROOT_DSNS="$dsns" \
    run_required_test TestQA484DeadlineMargin "$WORK/TestQA484DeadlineMargin.log" ./internal/meta
  local n
  n="$(grep -c -- '--- PASS: TestQA484DeadlineMargin/server-' "$WORK/TestQA484DeadlineMargin.log" || true)"
  if [[ "$n" -ne 2 ]]; then
    echo "TestQA484DeadlineMargin passed on ${n} servers, want 2" >&2
    return 1
  fi
  QA_PRE_HOLD_MS=1500 BINLOG_TEST_MYSQL_ROOT_DSNS="$dsns" \
    run_required_test TestQA484PoolPressure "$WORK/TestQA484PoolPressure.log" ./internal/meta
  n="$(grep -c -- '--- PASS: TestQA484PoolPressure/server-' "$WORK/TestQA484PoolPressure.log" || true)"
  if [[ "$n" -ne 2 ]]; then
    echo "TestQA484PoolPressure passed on ${n} servers, want 2" >&2
    return 1
  fi
  BINLOG_TEST_MYSQL_ROOT_DSNS="$dsns" \
    run_required_test TestRejectedSetRefusesStartup "$WORK/TestRejectedSetRefusesStartup.log" ./internal/meta
  n="$(grep -c -- '--- PASS: TestRejectedSetRefusesStartup' "$WORK/TestRejectedSetRefusesStartup.log" || true)"
  if [[ "$n" -lt 1 ]]; then
    echo "TestRejectedSetRefusesStartup did not pass" >&2
    return 1
  fi
  log "deadline margin, pool pressure, and a rejected SET passed on MySQL 8.0 and MariaDB"
}

cell_health() {
  # A scratch database with one stopped task. The suite meta already holds
  # tasks from earlier scenarios; evaluating those would degrade /api/health.
  local scratch="binlog_meta_sk${RUN_TAG}"
  SCRATCH_DB="$scratch"
  docker exec -i binlog-e2e-meta-primary mysql -uroot -proot -e \
    "DROP DATABASE IF EXISTS ${scratch}; CREATE DATABASE ${scratch} DEFAULT CHARACTER SET utf8mb4; GRANT ALL PRIVILEGES ON \`${scratch}\`.* TO 'meta'@'%'; FLUSH PRIVILEGES;"
  local dsn
  dsn="$(printf '%s' "$META_DSN" | sed -E "s#^(.*@tcp\\([^)]+\\)/)[^/?]+#\\1${scratch}#")"
  ( cd "$ROOT_DIR" && META_DSN="$dsn" go run ./cmd/migrate up )
  docker exec -i binlog-e2e-meta-primary mysql -uroot -proot "$scratch" -e \
    "INSERT INTO backup_tasks (id, name, cluster_key, state, last_error, owner_worker_id, epoch, run_id, source_json, start_json, storage_json, updated_at, desired_run, spec_revision, applied_spec_revision, failed_spec_revision, retry_attempt, consecutive_source_failures) VALUES ('sk-health', 'sk-health', 'sk-health-${RUN_TAG}', 'STOPPED', '', '', 0, '', '{}', '{}', '{}', UTC_TIMESTAMP(6), 'STOP', 1, 1, 0, 0, 0)"
  local tasks
  tasks="$(docker exec -i binlog-e2e-meta-primary mysql -uroot -proot -Nse "SELECT COUNT(*) FROM ${scratch}.backup_tasks")"
  [[ "$tasks" == "1" ]] || { echo "health cell has ${tasks} tasks, want 1" >&2; return 1; }
  local data="$WORK/data"
  mkdir -p "$data"
  (
    cd "$ROOT_DIR"
    exec env -u BINLOG_SERVER_HEALTH_SWEEP_INTERVAL_SEC \
      BINLOG_SERVER_MODE=cluster \
      BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18120 \
      BINLOG_SERVER_DATA_DIR="$data" \
      BINLOG_SERVER_META_DSN="$dsn" \
      "$BIN" --config "$ROOT_DIR/deploy/e2e/config.yaml"
  ) >"$WORK/health-server.log" 2>&1 &
  HEALTH_PID=$!
  local i
  for i in $(seq 1 60); do
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      break
    fi
    if ! kill -0 "$HEALTH_PID" 2>/dev/null; then
      echo "health server exited" >&2
      return 1
    fi
    if [[ "$i" -eq 60 ]]; then
      echo "health server not ready" >&2
      return 1
    fi
    sleep 0.5
  done
  if tr '\0' '\n' <"/proc/${HEALTH_PID}/environ" | grep -q '^BINLOG_SERVER_HEALTH_SWEEP_INTERVAL_SEC='; then
    echo "health sweep interval is overridden; the cell needs the 60s default" >&2
    return 1
  fi
  local start now samples=0
  start="$(date +%s)"
  while true; do
    assert_health
    samples=$((samples + 1))
    now="$(date +%s)"
    if (( now - start >= 65 )); then
      break
    fi
    sleep 5
  done
  if (( samples < 12 )); then
    echo "health cell took ${samples} samples, want at least 12 across one 60s sweep" >&2
    return 1
  fi
  log "one task and the default sweep kept /api/health at 200 for ${samples} samples"
}

assert_health() {
  local body code
  body="$(curl -sS -w '\n%{http_code}' "$API/api/health")"
  code="$(printf '%s' "$body" | tail -n 1)"
  body="$(printf '%s' "$body" | sed '$d')"
  if [[ "$code" != "200" ]]; then
    echo "/api/health $code, want 200: $body" >&2
    return 1
  fi
  if printf '%s' "$body" | grep -q META_LOCK_STALLED; then
    echo "/api/health named META_LOCK_STALLED: $body" >&2
    return 1
  fi
  if printf '%s' "$body" | grep -q HEALTH_STALE; then
    echo "/api/health named HEALTH_STALE: $body" >&2
    return 1
  fi
  local status
  status="$(printf '%s' "$body" | jq -r '.status')"
  [[ "$status" == "ok" ]] || { echo "/api/health status=$status: $body" >&2; return 1; }
}

cell_mysql80
start_mariadb
cell_mariadb
cell_pool_test
cell_qa_margin
cell_noprocess mysql80_sql "${E2E_MYSQL80_PORT}" mysql80
cell_noprocess maria_sql "${MARIA_PORT}" mariadb
cell_health
log "session-kill cells passed"
