#!/usr/bin/env bash
# input: compose meta-primary (Percona 5.7) and mysql80, both with log_bin on, and ./cmd/migrate
# output: proof that migrate up and down to schema 11 succeed for an account granted only ALL on a scratch schema, that innodb_trx is unreadable, that no trigger or procedure is created, and that a later up at version 11 does not warn
# pos: least-privilege acceptance cell for ADR 0011 migration 000011; runs in the quick profile
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
cd "$ROOT_DIR"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"

MIGRATE="${TMPDIR:-/tmp}/binlog-migrate-lp"
USER_NAME="lp_migrate"
USER_PASS="lppass"
CLEAN_SERVICES=()
CLEAN_DBS=()

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd docker
need_cmd go

fail() {
  echo "$1" >&2
  exit 1
}

mysql_root() {
  local service="$1"
  shift
  docker compose -f "$COMPOSE_FILE" exec -T -e MYSQL_PWD=root "$service" mysql -uroot "$@"
}

cleanup() {
  local i
  for i in "${!CLEAN_SERVICES[@]}"; do
    mysql_root "${CLEAN_SERVICES[$i]}" -e "DROP DATABASE IF EXISTS \`${CLEAN_DBS[$i]}\`; DROP USER IF EXISTS '${USER_NAME}'@'%';" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

assert_grants() {
  local service="$1" db="$2" grants
  grants="$(mysql_root "$service" -Nse "SHOW GRANTS FOR '${USER_NAME}'@'%'")"
  printf '%s\n' "$grants"
  if printf '%s\n' "$grants" | grep -Ei '(^|[^A-Z_])(PROCESS|SUPER|CONNECTION_ADMIN|REPLICATION|FILE|SHUTDOWN)([^A-Z_]|$)'; then
    fail "$service grants are wider than ALL on ${db}"
  fi
  printf '%s\n' "$grants" | grep -q "ALL PRIVILEGES ON" || fail "$service missing schema ALL"
  printf '%s\n' "$grants" | grep -F "$db" >/dev/null || fail "$service grant is not on ${db}"
  printf '%s\n' "$grants" | grep -q "USAGE ON" || fail "$service missing USAGE"
}

assert_log_bin() {
  local service="$1" line bin trust
  line="$(mysql_root "$service" -Nse "SELECT @@global.log_bin, @@global.log_bin_trust_function_creators" | tr -d '\r')"
  bin="$(printf '%s' "$line" | awk '{print $1}')"
  trust="$(printf '%s' "$line" | awk '{print $2}')"
  case "$bin" in
    1|ON|on) ;;
    *) fail "$service log_bin=$bin" ;;
  esac
  case "$trust" in
    0|OFF|off) ;;
    *) fail "$service log_bin_trust_function_creators=$trust" ;;
  esac
}

assert_trx_denied() {
  local service="$1" err
  err="$(mktemp)"
  if docker compose -f "$COMPOSE_FILE" exec -T -e MYSQL_PWD="$USER_PASS" "$service" \
    mysql -u"$USER_NAME" -Nse "SELECT COUNT(*) FROM information_schema.innodb_trx" >/dev/null 2>"$err"; then
    rm -f "$err"
    fail "$service account can read innodb_trx"
  fi
  if ! grep -Eq '1227|1142|PROCESS|denied|command denied' "$err"; then
    echo "unexpected innodb_trx error:" >&2
    cat "$err" >&2
    rm -f "$err"
    fail "$service innodb_trx denial was not a privilege error"
  fi
  rm -f "$err"
}

count_object() {
  local service="$1" query="$2"
  mysql_root "$service" -Nse "$query" | tr -d '[:space:]'
}

run_migrate() {
  local dsn="$1" log="$2"
  shift 2
  if ! "$MIGRATE" "$@" --dsn "$dsn" --path "$ROOT_DIR/migrations" >"$log" 2>&1; then
    echo "migrate failed:" >&2
    cat "$log" >&2
    fail "migrate $* failed"
  fi
}

run_one() {
  local service="$1" port="$2" db="$3"
  local dsn="${USER_NAME}:${USER_PASS}@tcp(127.0.0.1:${port})/${db}?parseTime=true"
  local work
  work="$(mktemp -d)"
  echo "[lp] ${service} port=${port} db=${db}"
  CLEAN_SERVICES+=("$service")
  CLEAN_DBS+=("$db")
  mysql_root "$service" -e "DROP DATABASE IF EXISTS \`${db}\`; DROP USER IF EXISTS '${USER_NAME}'@'%'; CREATE DATABASE \`${db}\`; CREATE USER '${USER_NAME}'@'%' IDENTIFIED BY '${USER_PASS}'; GRANT ALL ON \`${db}\`.* TO '${USER_NAME}'@'%'; FLUSH PRIVILEGES;"
  assert_grants "$service" "$db"
  assert_log_bin "$service"
  assert_trx_denied "$service"

  run_migrate "$dsn" "$work/up.log" up
  grep -q "skipped innodb_trx precheck" "$work/up.log" || fail "$service up did not warn about the skipped precheck"
  grep -q "PROCESS" "$work/up.log" || fail "$service up warning did not name PROCESS"
  grep -q "version=11 dirty=false" "$work/up.log" || fail "$service up version: $(cat "$work/up.log")"
  [[ "$(count_object "$service" "SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema='${db}' AND trigger_name='backup_tasks_bu_generation'")" == "0" ]] || fail "$service created a trigger"
  [[ "$(count_object "$service" "SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema='${db}' AND routine_name='binlog_server_kill_trx'")" == "0" ]] || fail "$service created a procedure"

  run_migrate "$dsn" "$work/down.log" down --steps 1
  grep -q "version=10 dirty=false" "$work/down.log" || fail "$service down version: $(cat "$work/down.log")"
  [[ "$(count_object "$service" "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='${db}' AND table_name='backup_tasks' AND column_name='row_generation'")" == "0" ]] || fail "$service down left row_generation"

  run_migrate "$dsn" "$work/up2.log" up
  grep -q "skipped innodb_trx precheck" "$work/up2.log" || fail "$service second up did not warn"
  grep -q "version=11 dirty=false" "$work/up2.log" || fail "$service second up version: $(cat "$work/up2.log")"

  run_migrate "$dsn" "$work/up3.log" up
  if grep -q "skipped innodb_trx precheck" "$work/up3.log"; then
    fail "$service warned again while already at version 11"
  fi
  grep -q "version=11 dirty=false" "$work/up3.log" || fail "$service idle up version: $(cat "$work/up3.log")"
  rm -rf "$work"
  echo "[lp] ${service} ok"
}

(
  cd "$ROOT_DIR"
  go build -o "$MIGRATE" ./cmd/migrate
)

run_one meta-primary "$E2E_META_PRIMARY_PORT" lp_migrate_meta
run_one mysql80 "$E2E_MYSQL80_PORT" lp_migrate_80
echo "[lp] least-privilege migrate up and down passed"
