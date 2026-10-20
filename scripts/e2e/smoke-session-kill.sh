#!/usr/bin/env bash
# input: docker mysql:8.0 (compose mysql80) and mariadb:11.4, the suite metadata DSN, curl and jq
# output: SIGSTOP of a process holding task_leases FOR UPDATE is released inside the session-kill bound on MySQL 8.0 and MariaDB; a cluster server with the default 60s sweep keeps /api/health at 200
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
HEALTH_PID=""
HOLDER_PID=""
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

prepare_row() {
  local sql_fn="$1" db="$2"
  "$sql_fn" "DROP DATABASE IF EXISTS ${db}; CREATE DATABASE ${db};"
  "$sql_fn" "CREATE TABLE ${db}.task_leases (
    task_id varchar(191) NOT NULL PRIMARY KEY,
    owner_worker_id varchar(191) NOT NULL,
    epoch bigint NOT NULL,
    lease_expire_at datetime(6) NOT NULL,
    renewed_at datetime(6) NOT NULL
  )"
  "$sql_fn" "INSERT INTO ${db}.task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
    VALUES ('t-stop', 'w1', 1, DATE_ADD(NOW(6), INTERVAL 1 HOUR), NOW(6))"
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
  prepare_row mysql80_sql "$db"
  local dsn="root:root@tcp(127.0.0.1:${E2E_MYSQL80_PORT})/${db}?parseTime=true&wait_timeout=2&net_read_timeout=2&net_write_timeout=2&innodb_lock_wait_timeout=2"
  local peer="root:root@tcp(127.0.0.1:${E2E_MYSQL80_PORT})/${db}?parseTime=true&innodb_lock_wait_timeout=8"
  stop_holder "$dsn" "$peer" mysql80
  mysql80_sql "DROP DATABASE IF EXISTS ${db}" || true
}

cell_mariadb() {
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
  local db="binlog_sk_${RUN_TAG}"
  prepare_row maria_sql "$db"
  local dsn="root:root@tcp(127.0.0.1:${MARIA_PORT})/${db}?parseTime=true&wait_timeout=2&net_read_timeout=2&net_write_timeout=2&innodb_lock_wait_timeout=2&idle_transaction_timeout=2"
  local peer="root:root@tcp(127.0.0.1:${MARIA_PORT})/${db}?parseTime=true&innodb_lock_wait_timeout=8"
  stop_holder "$dsn" "$peer" mariadb
}

cell_health() {
  # An empty database. The suite meta already holds tasks from earlier
  # scenarios; evaluating those would degrade /api/health for other reasons.
  local scratch="binlog_meta_sk${RUN_TAG}"
  SCRATCH_DB="$scratch"
  docker exec -i binlog-e2e-meta-primary mysql -uroot -proot -e \
    "DROP DATABASE IF EXISTS ${scratch}; CREATE DATABASE ${scratch} DEFAULT CHARACTER SET utf8mb4; GRANT ALL PRIVILEGES ON \`${scratch}\`.* TO 'meta'@'%'; FLUSH PRIVILEGES;"
  local dsn
  dsn="$(printf '%s' "$META_DSN" | sed -E "s#^(.*@tcp\\([^)]+\\)/)[^/?]+#\\1${scratch}#")"
  ( cd "$ROOT_DIR" && META_DSN="$dsn" go run ./cmd/migrate up )
  local data="$WORK/data"
  mkdir -p "$data"
  (
    cd "$ROOT_DIR"
    exec env -u BINLOG_SERVER_HEALTH_SWEEP_INTERVAL_SEC \
      BINLOG_SERVER_MODE=cluster \
      BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18120 \
      BINLOG_SERVER_DATA_DIR="$data" \
      BINLOG_SERVER_META_DSN="$dsn" \
      BINLOG_SERVER_HEALTH_EVALUATOR=on \
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
  assert_health
  sleep 5
  assert_health
  log "default sweep kept /api/health at 200"
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
  local status
  status="$(printf '%s' "$body" | jq -r '.status')"
  [[ "$status" == "ok" ]] || { echo "/api/health status=$status: $body" >&2; return 1; }
}

cell_mysql80
cell_mariadb
cell_health
log "session-kill cells passed"
