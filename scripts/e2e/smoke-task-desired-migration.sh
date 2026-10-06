#!/usr/bin/env bash
# input: meta-primary MySQL, migration 000003, and the current binlog-server binary
# output: proof that up lands on schema (3,0), backfills desired_run, the current binary starts on schema 3, refuses schema 2 with ./migrate up, and down returns to version 2 without deleting rows
# pos: acceptance check for the task-desired migration and the schema 3 startup gate
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-task-desired-$(date +%s)}"
SERVER_LOG="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-task-desired.log}"
SERVER_PID=""
SCRATCH_DB="binlog_meta_m000003"

source "$ROOT_DIR/scripts/e2e/lib-migration.sh"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd go

SCRATCH_DSN="$(e2e_meta_dsn direct | sed -E "s#^(.*@tcp\\([^)]+\\)/)[^/?]+#\\1${SCRATCH_DB}#")"

kill_server() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" >/dev/null 2>&1 || true
    SERVER_PID=""
  fi
}

cleanup() {
  kill_server
}
trap cleanup EXIT

meta_exec() {
  docker compose -f "$COMPOSE_FILE" exec -T meta-primary \
    mysql -uroot -proot "$@"
}

meta_sql() {
  meta_exec "$SCRATCH_DB" -Nse "$1" | tr -d '\r'
}

sql_one() {
  printf '%s' "$(meta_sql "$1")" | tr -d '[:space:]'
}

fail() {
  echo "$1" >&2
  exit 1
}

if ! grep -q 'const minRequiredSchemaVersion int64 = 3' "$ROOT_DIR/internal/meta/mysql_store.go"; then
  fail "minRequiredSchemaVersion must be 3"
fi

echo "[task-desired] scratch database $SCRATCH_DB"
meta_exec -e "DROP DATABASE IF EXISTS ${SCRATCH_DB}; CREATE DATABASE ${SCRATCH_DB} DEFAULT CHARACTER SET utf8mb4; GRANT ALL PRIVILEGES ON ${SCRATCH_DB}.* TO 'meta'@'%'; FLUSH PRIVILEGES;"

(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate goto 2
)

seed_file="$(mktemp)"
cat >"$seed_file" <<'EOF'
INSERT INTO backup_tasks (id, name, cluster_key, state, last_error, owner_worker_id, epoch, run_id, source_json, start_json, storage_json, updated_at)
VALUES
('m3-RUNNING', 'm3-RUNNING', 'm3-RUNNING', 'RUNNING', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6)),
('m3-STARTING', 'm3-STARTING', 'm3-STARTING', 'STARTING', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6)),
('m3-RETRY_BACKOFF', 'm3-RETRY_BACKOFF', 'm3-RETRY_BACKOFF', 'RETRY_BACKOFF', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6)),
('m3-LEASE_DEGRADED', 'm3-LEASE_DEGRADED', 'm3-LEASE_DEGRADED', 'LEASE_DEGRADED', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6)),
('m3-REBUILDING_FILE', 'm3-REBUILDING_FILE', 'm3-REBUILDING_FILE', 'REBUILDING_FILE', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6)),
('m3-CREATED', 'm3-CREATED', 'm3-CREATED', 'CREATED', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6)),
('m3-STOPPING', 'm3-STOPPING', 'm3-STOPPING', 'STOPPING', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6)),
('m3-STOPPED', 'm3-STOPPED', 'm3-STOPPED', 'STOPPED', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6)),
('m3-FAILED', 'm3-FAILED', 'm3-FAILED', 'FAILED', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6));
EOF
meta_exec "$SCRATCH_DB" <"$seed_file"
rm -f "$seed_file"

count_before="$(sql_one "SELECT COUNT(*) FROM backup_tasks")"
files_before="$(sql_one "SELECT COUNT(*) FROM binlog_files")"
if [[ "$count_before" != "9" || "$files_before" != "0" ]]; then
  fail "seed count tasks=$count_before files=$files_before"
fi

(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate up
)

version_row="$(meta_sql "SELECT version, dirty FROM schema_migrations")"
version_row="$(printf '%s' "$version_row" | tr -d '[:space:]')"
if [[ "$version_row" != "30" ]]; then
  fail "schema_migrations after up: [$version_row] want 3 0"
fi

show_columns="$(meta_sql "SHOW COLUMNS FROM backup_tasks")"
for col in desired_run spec_revision applied_spec_revision failed_spec_revision retry_attempt consecutive_source_failures; do
  if ! printf '%s\n' "$show_columns" | awk -F'\t' -v c="$col" '$1==c { found=1 } END { exit found ? 0 : 1 }'; then
    echo "$show_columns" >&2
    fail "SHOW COLUMNS missing $col"
  fi
done

nulls="$(sql_one "SELECT COUNT(*) FROM backup_tasks WHERE desired_run IS NULL OR spec_revision IS NULL OR applied_spec_revision IS NULL OR failed_spec_revision IS NULL OR retry_attempt IS NULL OR consecutive_source_failures IS NULL")"
if [[ "$nulls" != "0" ]]; then
  fail "null new columns: $nulls"
fi

column_defs="$(meta_sql "SELECT COLUMN_NAME, IS_NULLABLE, COLUMN_DEFAULT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'backup_tasks' AND COLUMN_NAME IN ('desired_run','spec_revision','applied_spec_revision','failed_spec_revision','retry_attempt','consecutive_source_failures') ORDER BY COLUMN_NAME")"
expected_defs=$'applied_spec_revision\tNO\t0\nconsecutive_source_failures\tNO\t0\ndesired_run\tNO\tSTOP\nfailed_spec_revision\tNO\t0\nretry_attempt\tNO\t0\nspec_revision\tNO\t0'
if [[ "$column_defs" != "$expected_defs" ]]; then
  printf 'column defs:\n%s\n' "$column_defs" >&2
  fail "column nullability or defaults mismatch"
fi

mapping="$(meta_sql "SELECT CONCAT(id, ' ', desired_run, ' ', spec_revision, ' ', applied_spec_revision, ' ', failed_spec_revision, ' ', retry_attempt, ' ', consecutive_source_failures) FROM backup_tasks ORDER BY id")"
expected_mapping=$'m3-CREATED STOP 0 0 0 0 0\nm3-FAILED STOP 0 0 0 0 0\nm3-LEASE_DEGRADED RUN 0 0 0 0 0\nm3-REBUILDING_FILE RUN 0 0 0 0 0\nm3-RETRY_BACKOFF RUN 0 0 0 0 0\nm3-RUNNING RUN 0 0 0 0 0\nm3-STARTING RUN 0 0 0 0 0\nm3-STOPPED STOP 0 0 0 0 0\nm3-STOPPING STOP 0 0 0 0 0'
if [[ "$mapping" != "$expected_mapping" ]]; then
  printf 'mapping:\n%s\n' "$mapping" >&2
  fail "desired_run backfill mismatch"
fi

count_after_up="$(sql_one "SELECT COUNT(*) FROM backup_tasks")"
if [[ "$count_after_up" != "$count_before" ]]; then
  fail "up changed backup_tasks count $count_before -> $count_after_up"
fi

# The previous release lists these columns and no others. A duplicate insert must leave desired_run alone.
meta_exec "$SCRATCH_DB" -e "INSERT INTO backup_tasks (id, name, cluster_key, state, last_error, owner_worker_id, epoch, run_id, source_json, start_json, storage_json, updated_at) VALUES ('m3-RUNNING', 'm3-RUNNING', 'm3-RUNNING', 'RUNNING', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE name = VALUES(name), cluster_key = VALUES(cluster_key), state = VALUES(state), last_error = VALUES(last_error), owner_worker_id = VALUES(owner_worker_id), epoch = VALUES(epoch), run_id = VALUES(run_id), source_json = VALUES(source_json), start_json = VALUES(start_json), storage_json = VALUES(storage_json), updated_at = VALUES(updated_at)"
kept="$(sql_one "SELECT desired_run FROM backup_tasks WHERE id='m3-RUNNING'")"
if [[ "$kept" != "RUN" ]]; then
  fail "old upsert cleared desired_run: $kept"
fi

meta_exec "$SCRATCH_DB" -e "INSERT INTO backup_tasks (id, name, cluster_key, state, last_error, owner_worker_id, epoch, run_id, source_json, start_json, storage_json, updated_at) VALUES ('m3-old-binary', 'm3-old-binary', 'm3-old-binary', 'CREATED', NULL, NULL, 0, NULL, '{}', '{}', '{}', UTC_TIMESTAMP(6))"
old_row="$(sql_one "SELECT CONCAT(desired_run, spec_revision, applied_spec_revision, failed_spec_revision, retry_attempt, consecutive_source_failures) FROM backup_tasks WHERE id='m3-old-binary'")"
if [[ "$old_row" != "STOP00000" ]]; then
  fail "old insert defaults: $old_row"
fi

count_before_down="$(sql_one "SELECT COUNT(*) FROM backup_tasks")"
files_before_down="$(sql_one "SELECT COUNT(*) FROM binlog_files")"

echo "[task-desired] start current binary against schema 3"
mkdir -p "$DATA_DIR"
BINLOG_SERVER_DATA_DIR="$DATA_DIR" \
BINLOG_SERVER_META_DSN="$SCRATCH_DSN" \
BINLOG_SERVER_MODE=cluster \
BINLOG_SERVER_CLUSTER_ROLE=control-plane \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
ready=0
for _ in {1..120}; do
  if curl -fsS "$API/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "$SERVER_PID" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if [[ "$ready" != "1" ]]; then
  echo "binary did not pass healthz on schema 3" >&2
  cat "$SERVER_LOG" >&2 || true
  exit 1
fi
kill_server

count_after_start="$(sql_one "SELECT COUNT(*) FROM backup_tasks")"
if [[ "$count_after_start" != "$count_before_down" ]]; then
  fail "process changed backup_tasks count $count_before_down -> $count_after_start"
fi

(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate down --steps 1
)

version_down="$(meta_sql "SELECT version, dirty FROM schema_migrations")"
version_down="$(printf '%s' "$version_down" | tr -d '[:space:]')"
if [[ "$version_down" != "20" ]]; then
  fail "schema_migrations after down: [$version_down] want 2 0"
fi

count_after_down="$(sql_one "SELECT COUNT(*) FROM backup_tasks")"
files_after_down="$(sql_one "SELECT COUNT(*) FROM binlog_files")"
if [[ "$count_after_down" != "$count_before_down" || "$files_after_down" != "$files_before_down" ]]; then
  fail "down changed counts tasks $count_before_down->$count_after_down files $files_before_down->$files_after_down"
fi
if [[ "$(sql_one "SELECT COUNT(*) FROM backup_tasks WHERE id='m3-old-binary'")" != "1" ]]; then
  fail "down deleted the old-binary row"
fi

gone="$(sql_one "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'backup_tasks' AND COLUMN_NAME IN ('desired_run','spec_revision','applied_spec_revision','failed_spec_revision','retry_attempt','consecutive_source_failures')")"
if [[ "$gone" != "0" ]]; then
  fail "down left $gone new columns"
fi

echo "[task-desired] current binary must refuse schema 2"
REFUSE_DIR="${DATA_DIR}-schema2"
REFUSE_LOG="${SERVER_LOG}.schema2"
mkdir -p "$REFUSE_DIR"
: >"$REFUSE_LOG"
BINLOG_SERVER_DATA_DIR="$REFUSE_DIR" \
BINLOG_SERVER_META_DSN="$SCRATCH_DSN" \
BINLOG_SERVER_MODE=cluster \
BINLOG_SERVER_CLUSTER_ROLE=control-plane \
  nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$REFUSE_LOG" 2>&1 &
SERVER_PID=$!
refused=0
for _ in $(seq 1 30); do
  if ! kill -0 "$SERVER_PID" >/dev/null 2>&1; then
    refused=1
    break
  fi
  if curl -fsS "$API/healthz" >/dev/null 2>&1; then
    fail "schema 2 passed healthz"
  fi
  sleep 1
done
kill_server
if [[ "$refused" != "1" ]]; then
  cat "$REFUSE_LOG" >&2 || true
  fail "schema 2 process still running"
fi
if ! grep -q '\./migrate up' "$REFUSE_LOG"; then
  cat "$REFUSE_LOG" >&2 || true
  fail "schema 2 log did not tell the operator to run ./migrate up"
fi

meta_exec -e "DROP DATABASE IF EXISTS ${SCRATCH_DB};" >/dev/null
echo "[task-desired] success: schema 3 backfill, healthz on schema 3, refuse schema 2, down keeps rows"
