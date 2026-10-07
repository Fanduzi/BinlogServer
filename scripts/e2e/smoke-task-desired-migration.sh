#!/usr/bin/env bash
# input: meta-primary MySQL, a MySQL 8 server, migrations through 000006, the current binlog-server binary, and the published v0.5.49 and v0.5.52 binaries
# output: proof that 000005 still adds both unique keys, the current binary refuses schema 5, 000006 drops uk_task_file_epoch without deleting rows, the current binary starts on schema 6 and seals a task, v0.5.52 refuses schema 6 with missing index uk_task_file_epoch, and down restores that index with the binlog_files count unchanged
# pos: acceptance check for ADR 0005 step 9 on a real metadata database
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
need_cmd jq

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

compose_mysql() {
  local service="$1"
  shift
  docker compose -f "$COMPOSE_FILE" exec -T "$service" mysql -uroot -proot "$@"
}

service_sql() {
  local service="$1" db="$2" query="$3"
  compose_mysql "$service" "$db" -Nse "$query" | tr -d '\r'
}

service_one() {
  printf '%s' "$(service_sql "$@")" | tr -d '[:space:]'
}

seed_binlog_segments() {
  local service="$1" db="$2"
  compose_mysql "$service" "$db" <<'EOF'
INSERT INTO binlog_files (
  task_id, file_name, source_file, file_path, epoch, state, size_bytes, start_pos, end_pos, created_at, sealed_at, upload_state
) VALUES
('seg-plain', 'mysql-bin.000001', 'mysql-bin.000001', '/data/mysql-bin.000001', 0, 'SEALED', 100, 4, 100, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'LOCAL_ONLY'),
('seg-empty', 'mysql-bin.000009', '', '/data/mysql-bin.000009', 1, 'SEALED', 50, 4, 0, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'LOCAL_ONLY'),
('seg-null', 'mysql-bin.000010', NULL, '/data/mysql-bin.000010', 0, 'SEALED', 80, 4, 200, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'LOCAL_ONLY'),
('seg-epoch', 'mysql-bin.000001', 'mysql-bin.000001', '/data/mysql-bin.000001.e2', 2, 'SEALED', 90, 4, 543, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'LOCAL_ONLY');
EOF
}

segment_positions() {
  service_sql "$1" "$2" "SELECT CONCAT(task_id, ':', epoch, ':', start_pos, ':', end_pos) FROM binlog_files WHERE task_id IN ('seg-plain','seg-empty','seg-null','seg-epoch') ORDER BY task_id, epoch"
}

assert_schema5_catalog() {
  local label="$1" service="$2" db="$3" before_count="$4" before_positions="$5"
  local version nulls mismatch count positions indexes nullable dup_err
  version="$(service_one "$service" "$db" "SELECT version, dirty FROM schema_migrations")"
  if [[ "$version" != "50" ]]; then
    fail "$label schema_migrations after 000005: [$version] want 5 0"
  fi
  nulls="$(service_one "$service" "$db" "SELECT COUNT(*) FROM binlog_files WHERE source_file IS NULL OR source_file=''")"
  if [[ "$nulls" != "0" ]]; then
    fail "$label empty source_file count $nulls"
  fi
  mismatch="$(service_one "$service" "$db" "SELECT COUNT(*) FROM binlog_files WHERE source_file <> file_name")"
  if [[ "$mismatch" != "0" ]]; then
    fail "$label source_file <> file_name count $mismatch"
  fi
  count="$(service_one "$service" "$db" "SELECT COUNT(*) FROM binlog_files")"
  if [[ "$count" != "$before_count" ]]; then
    fail "$label up changed binlog_files count $before_count -> $count"
  fi
  positions="$(segment_positions "$service" "$db")"
  if [[ "$positions" != "$before_positions" ]]; then
    printf '%s positions before:\n%s\nafter:\n%s\n' "$label" "$before_positions" "$positions" >&2
    fail "$label start_pos/end_pos changed"
  fi
  indexes="$(service_sql "$service" "$db" "SELECT INDEX_NAME, NON_UNIQUE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND INDEX_NAME IN ('uk_task_file_epoch','uk_task_source_epoch') GROUP BY INDEX_NAME, NON_UNIQUE ORDER BY INDEX_NAME")"
  if [[ "$indexes" != $'uk_task_file_epoch\t0\nuk_task_source_epoch\t0' ]]; then
    printf '%s indexes:\n%s\n' "$label" "$indexes" >&2
    fail "$label missing one of the unique keys"
  fi
  show_index="$(service_sql "$service" "$db" "SHOW INDEX FROM binlog_files")"
  if ! printf '%s\n' "$show_index" | awk -F'\t' '$3=="uk_task_file_epoch" && $2=="0" { found=1 } END { exit found ? 0 : 1 }'; then
    printf '%s SHOW INDEX:\n%s\n' "$label" "$show_index" >&2
    fail "$label SHOW INDEX missing uk_task_file_epoch"
  fi
  if ! printf '%s\n' "$show_index" | awk -F'\t' '$3=="uk_task_source_epoch" && $2=="0" { found=1 } END { exit found ? 0 : 1 }'; then
    printf '%s SHOW INDEX:\n%s\n' "$label" "$show_index" >&2
    fail "$label SHOW INDEX missing uk_task_source_epoch"
  fi
  nullable="$(service_sql "$service" "$db" "SELECT COLUMN_NAME, IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND COLUMN_NAME IN ('source_file','start_pos','end_pos') ORDER BY COLUMN_NAME")"
  if [[ "$nullable" != $'end_pos\tYES\nsource_file\tNO\nstart_pos\tYES' ]]; then
    printf '%s nullability:\n%s\n' "$label" "$nullable" >&2
    fail "$label nullability mismatch"
  fi
  dup_err="$(mktemp)"
  if compose_mysql "$service" "$db" -e "INSERT INTO binlog_files (task_id, file_name, source_file, file_path, epoch, state, size_bytes, start_pos, end_pos, created_at, sealed_at, upload_state) VALUES ('seg-plain', 'mysql-bin.OTHER', 'mysql-bin.000001', '/data/other', 0, 'SEALED', 1, 4, 9, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'LOCAL_ONLY')" >"$dup_err" 2>&1; then
    cat "$dup_err" >&2
    rm -f "$dup_err"
    fail "$label duplicate (task_id, source_file, epoch) insert succeeded"
  fi
  if ! grep -Eq 'Duplicate entry|ERROR 1062' "$dup_err"; then
    cat "$dup_err" >&2
    rm -f "$dup_err"
    fail "$label duplicate insert was not a duplicate key"
  fi
  rm -f "$dup_err"
  count="$(service_one "$service" "$db" "SELECT COUNT(*) FROM binlog_files")"
  if [[ "$count" != "$before_count" ]]; then
    fail "$label duplicate insert changed count $before_count -> $count"
  fi
}

assert_schema4_after_down() {
  local label="$1" service="$2" db="$3" before_count="$4"
  local version count indexes nullable
  version="$(service_one "$service" "$db" "SELECT version, dirty FROM schema_migrations")"
  if [[ "$version" != "40" ]]; then
    fail "$label schema_migrations after down: [$version] want 4 0"
  fi
  count="$(service_one "$service" "$db" "SELECT COUNT(*) FROM binlog_files")"
  if [[ "$count" != "$before_count" ]]; then
    fail "$label down changed binlog_files count $before_count -> $count"
  fi
  indexes="$(service_sql "$service" "$db" "SELECT INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND INDEX_NAME IN ('uk_task_file_epoch','uk_task_source_epoch') GROUP BY INDEX_NAME ORDER BY INDEX_NAME")"
  if [[ "$indexes" != "uk_task_file_epoch" ]]; then
    printf '%s indexes after down:\n%s\n' "$label" "$indexes" >&2
    fail "$label down left uk_task_source_epoch or dropped uk_task_file_epoch"
  fi
  nullable="$(service_sql "$service" "$db" "SELECT COLUMN_NAME, IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND COLUMN_NAME IN ('source_file','start_pos','end_pos') ORDER BY COLUMN_NAME")"
  if [[ "$nullable" != $'end_pos\tNO\nsource_file\tYES\nstart_pos\tNO' ]]; then
    printf '%s nullability after down:\n%s\n' "$label" "$nullable" >&2
    fail "$label down did not restore nullability"
  fi
}

start_current() {
  local dsn="$1" data_dir="$2" log="$3"
  mkdir -p "$data_dir"
  : >"$log"
  BINLOG_SERVER_DATA_DIR="$data_dir" \
  BINLOG_SERVER_META_DSN="$dsn" \
  BINLOG_SERVER_MODE=cluster \
  BINLOG_SERVER_CLUSTER_ROLE=control-plane \
    nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$log" 2>&1 &
  SERVER_PID=$!
}

wait_healthz() {
  local log="$1" label="$2"
  local ready=0
  for _ in $(seq 1 120); do
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
    echo "$label did not pass healthz" >&2
    cat "$log" >&2 || true
    exit 1
  fi
}

expect_refuse() {
  local dsn="$1" data_dir="$2" log="$3" label="$4"
  local refused=0 rc=0
  mkdir -p "$data_dir"
  : >"$log"
  BINLOG_SERVER_DATA_DIR="$data_dir" \
  BINLOG_SERVER_META_DSN="$dsn" \
  BINLOG_SERVER_MODE=cluster \
  BINLOG_SERVER_CLUSTER_ROLE=control-plane \
    nohup "$ROOT_DIR/scripts/e2e/run-server.sh" >"$log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 60); do
    if ! kill -0 "$SERVER_PID" >/dev/null 2>&1; then
      refused=1
      break
    fi
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      fail "$label passed healthz"
    fi
    sleep 1
  done
  if [[ "$refused" != "1" ]]; then
    kill_server
    cat "$log" >&2 || true
    fail "$label process still running"
  fi
  set +e
  wait "$SERVER_PID"
  rc=$?
  set -e
  SERVER_PID=""
  if [[ "$rc" != "1" ]]; then
    cat "$log" >&2 || true
    fail "$label exit $rc, want 1"
  fi
  if ! grep -q 'schema version too old' "$log" || ! grep -q '\./migrate up' "$log"; then
    cat "$log" >&2 || true
    fail "$label log missing schema version too old or ./migrate up"
  fi
}

download_release() {
  local ver="$1"
  local dest="/tmp/binlog-server-v${ver}-linux-amd64"
  local archive="/tmp/binlog-server_${ver}_linux_amd64.tar.gz"
  if [[ -x "$dest/binlog-server" ]]; then
    printf '%s\n' "$dest/binlog-server"
    return 0
  fi
  rm -rf "$dest"
  mkdir -p "$dest"
  curl -fsSL -o "$archive" "https://github.com/Fanduzi/BinlogServer/releases/download/v${ver}/binlog-server_${ver}_linux_amd64.tar.gz"
  tar -xzf "$archive" -C "$dest" --strip-components=1
  if [[ ! -x "$dest/binlog-server" ]]; then
    chmod +x "$dest/binlog-server"
  fi
  printf '%s\n' "$dest/binlog-server"
}

assert_schema6_catalog() {
  local label="$1" service="$2" db="$3" before_count="$4"
  local version count indexes show_index file_col dup_err
  version="$(service_one "$service" "$db" "SELECT version, dirty FROM schema_migrations")"
  if [[ "$version" != "60" ]]; then
    fail "$label schema_migrations after 000006: [$version] want 6 0"
  fi
  count="$(service_one "$service" "$db" "SELECT COUNT(*) FROM binlog_files")"
  if [[ "$count" != "$before_count" ]]; then
    fail "$label 000006 changed binlog_files count $before_count -> $count"
  fi
  file_col="$(service_one "$service" "$db" "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND COLUMN_NAME = 'file_name'")"
  if [[ "$file_col" != "1" ]]; then
    fail "$label file_name column count $file_col"
  fi
  indexes="$(service_sql "$service" "$db" "SELECT INDEX_NAME, NON_UNIQUE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND INDEX_NAME IN ('uk_task_file_epoch','uk_task_source_epoch') GROUP BY INDEX_NAME, NON_UNIQUE ORDER BY INDEX_NAME")"
  if [[ "$indexes" != $'uk_task_source_epoch\t0' ]]; then
    printf '%s indexes:\n%s\n' "$label" "$indexes" >&2
    fail "$label SHOW-equivalent indexes are not only uk_task_source_epoch"
  fi
  show_index="$(service_sql "$service" "$db" "SHOW INDEX FROM binlog_files")"
  if printf '%s\n' "$show_index" | awk -F'\t' '$3=="uk_task_file_epoch" { found=1 } END { exit found ? 0 : 1 }'; then
    printf '%s SHOW INDEX:\n%s\n' "$label" "$show_index" >&2
    fail "$label SHOW INDEX still has uk_task_file_epoch"
  fi
  if ! printf '%s\n' "$show_index" | awk -F'\t' '$3=="uk_task_source_epoch" && $2=="0" { found=1 } END { exit found ? 0 : 1 }'; then
    printf '%s SHOW INDEX:\n%s\n' "$label" "$show_index" >&2
    fail "$label SHOW INDEX missing uk_task_source_epoch"
  fi
  dup_err="$(mktemp)"
  if compose_mysql "$service" "$db" -e "INSERT INTO binlog_files (task_id, file_name, source_file, file_path, epoch, state, size_bytes, start_pos, end_pos, created_at, sealed_at, upload_state) VALUES ('seg-plain', 'mysql-bin.OTHER', 'mysql-bin.000001', '/data/other', 0, 'SEALED', 1, 4, 9, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'LOCAL_ONLY')" >"$dup_err" 2>&1; then
    cat "$dup_err" >&2
    rm -f "$dup_err"
    fail "$label duplicate (task_id, source_file, epoch) insert succeeded"
  fi
  if ! grep -Eq 'Duplicate entry|ERROR 1062' "$dup_err"; then
    cat "$dup_err" >&2
    rm -f "$dup_err"
    fail "$label duplicate insert was not a duplicate key"
  fi
  rm -f "$dup_err"
  count="$(service_one "$service" "$db" "SELECT COUNT(*) FROM binlog_files")"
  if [[ "$count" != "$before_count" ]]; then
    fail "$label duplicate insert changed count $before_count -> $count"
  fi
}

assert_file_epoch_restored() {
  local label="$1" service="$2" db="$3" before_count="$4"
  local version count indexes file_col
  version="$(service_one "$service" "$db" "SELECT version, dirty FROM schema_migrations")"
  if [[ "$version" != "50" ]]; then
    fail "$label schema_migrations after down 000006: [$version] want 5 0"
  fi
  count="$(service_one "$service" "$db" "SELECT COUNT(*) FROM binlog_files")"
  if [[ "$count" != "$before_count" ]]; then
    fail "$label down 000006 changed binlog_files count $before_count -> $count"
  fi
  file_col="$(service_one "$service" "$db" "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND COLUMN_NAME = 'file_name'")"
  if [[ "$file_col" != "1" ]]; then
    fail "$label down dropped file_name"
  fi
  indexes="$(service_sql "$service" "$db" "SELECT INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'binlog_files' AND INDEX_NAME IN ('uk_task_file_epoch','uk_task_source_epoch') GROUP BY INDEX_NAME ORDER BY INDEX_NAME")"
  if [[ "$indexes" != $'uk_task_file_epoch\nuk_task_source_epoch' ]]; then
    printf '%s indexes after down 000006:\n%s\n' "$label" "$indexes" >&2
    fail "$label down did not restore uk_task_file_epoch"
  fi
}

expect_missing_file_epoch() {
  local bin="$1" dsn="$2" data_dir="$3" log="$4" label="$5"
  local refused=0 rc=0
  mkdir -p "$data_dir"
  : >"$log"
  BINLOG_SERVER_DATA_DIR="$data_dir" \
  BINLOG_SERVER_META_DSN="$dsn" \
  BINLOG_SERVER_MODE=cluster \
  BINLOG_SERVER_CLUSTER_ROLE=control-plane \
    nohup "$bin" --config "$ROOT_DIR/deploy/e2e/config.yaml" >"$log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 60); do
    if ! kill -0 "$SERVER_PID" >/dev/null 2>&1; then
      refused=1
      break
    fi
    if curl -fsS "$API/healthz" >/dev/null 2>&1; then
      fail "$label passed healthz"
    fi
    sleep 1
  done
  if [[ "$refused" != "1" ]]; then
    kill_server
    cat "$log" >&2 || true
    fail "$label process still running"
  fi
  set +e
  wait "$SERVER_PID"
  rc=$?
  set -e
  SERVER_PID=""
  if [[ "$rc" != "1" ]]; then
    cat "$log" >&2 || true
    fail "$label exit $rc, want 1"
  fi
  if ! grep -q 'missing index' "$log" || ! grep -q 'uk_task_file_epoch' "$log"; then
    cat "$log" >&2 || true
    fail "$label log missing index uk_task_file_epoch"
  fi
}

if ! grep -q 'const minRequiredSchemaVersion int64 = 6' "$ROOT_DIR/internal/meta/mysql_store.go"; then
  fail "minRequiredSchemaVersion must be 6"
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
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate goto 4
)

version_row="$(meta_sql "SELECT version, dirty FROM schema_migrations")"
version_row="$(printf '%s' "$version_row" | tr -d '[:space:]')"
if [[ "$version_row" != "40" ]]; then
  fail "schema_migrations after up: [$version_row] want 4 0"
fi

show_columns="$(meta_sql "SHOW COLUMNS FROM backup_tasks")"
for col in desired_run spec_revision applied_spec_revision failed_spec_revision retry_attempt consecutive_source_failures pending_dump_cleanup; do
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
pending_default="$(sql_one "SELECT pending_dump_cleanup FROM backup_tasks WHERE id='m3-old-binary'")"
if [[ -n "$pending_default" ]]; then
  fail "pending_dump_cleanup default: [$pending_default]"
fi

echo "[task-desired] seed schema-4 binlog_files, including an empty source_file and epoch>0"
seed_binlog_segments meta-primary "$SCRATCH_DB"
files_before_000005="$(sql_one "SELECT COUNT(*) FROM binlog_files")"
positions_before="$(segment_positions meta-primary "$SCRATCH_DB")"
if [[ "$files_before_000005" != "4" ]]; then
  fail "binlog seed count $files_before_000005"
fi
if [[ "$positions_before" != $'seg-empty:1:4:0\nseg-epoch:2:4:543\nseg-null:0:4:200\nseg-plain:0:4:100' ]]; then
  printf 'seed positions:\n%s\n' "$positions_before" >&2
  fail "binlog seed positions"
fi

echo "[task-desired] goto 5 on Percona 5.7"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate goto 5
)
assert_schema5_catalog percona meta-primary "$SCRATCH_DB" "$files_before_000005" "$positions_before"

echo "[task-desired] current binary must refuse schema 5"
expect_refuse "$SCRATCH_DSN" "${DATA_DIR}-schema5" "${SERVER_LOG}.schema5" "schema 5"

echo "[task-desired] migrate up 000006 on Percona 5.7"
files_before_000006="$(sql_one "SELECT COUNT(*) FROM binlog_files")"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate up
)
assert_schema6_catalog percona meta-primary "$SCRATCH_DB" "$files_before_000006"

echo "[task-desired] start current binary against schema 6"
start_current "$SCRATCH_DSN" "$DATA_DIR" "$SERVER_LOG"
wait_healthz "$SERVER_LOG" "schema 6"
kill_server

echo "[task-desired] down 000006; uk_task_file_epoch returns and binlog_files count stays"
files_before_down006="$(sql_one "SELECT COUNT(*) FROM binlog_files")"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate down --steps 1
)
assert_file_epoch_restored percona meta-primary "$SCRATCH_DB" "$files_before_down006"

echo "[task-desired] down 000005; binlog_files count stays"
files_before_down005="$(sql_one "SELECT COUNT(*) FROM binlog_files")"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate down --steps 1
)
assert_schema4_after_down percona meta-primary "$SCRATCH_DB" "$files_before_down005"

echo "[task-desired] current binary must refuse schema 4"
expect_refuse "$SCRATCH_DSN" "${DATA_DIR}-schema4" "${SERVER_LOG}.schema4" "schema 4"

count_before_down="$(sql_one "SELECT COUNT(*) FROM backup_tasks")"
files_before_down="$(sql_one "SELECT COUNT(*) FROM binlog_files")"

echo "[task-desired] down 000004 only; schema 3 still has the desired-run columns"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$SCRATCH_DSN" go run ./cmd/migrate down --steps 1
)
version_pending="$(meta_sql "SELECT version, dirty FROM schema_migrations")"
version_pending="$(printf '%s' "$version_pending" | tr -d '[:space:]')"
if [[ "$version_pending" != "30" ]]; then
  fail "schema_migrations after dropping 000004: [$version_pending] want 3 0"
fi
pending_left="$(sql_one "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'backup_tasks' AND COLUMN_NAME = 'pending_dump_cleanup'")"
desired_left="$(sql_one "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'backup_tasks' AND COLUMN_NAME = 'desired_run'")"
if [[ "$pending_left" != "0" || "$desired_left" != "1" ]]; then
  fail "000004 down pending=$pending_left desired_run=$desired_left"
fi
if [[ "$(sql_one "SELECT COUNT(*) FROM backup_tasks")" != "$count_before_down" || "$(sql_one "SELECT COUNT(*) FROM binlog_files")" != "$files_before_down" ]]; then
  fail "000004 down changed row counts"
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
expect_refuse "$SCRATCH_DSN" "${DATA_DIR}-schema2" "${SERVER_LOG}.schema2" "schema 2"

meta_exec -e "DROP DATABASE IF EXISTS ${SCRATCH_DB};" >/dev/null

MYSQL80_DB="binlog_meta_m000005"
MYSQL80_PORT="${E2E_MYSQL80_PORT:-13307}"
MYSQL80_DSN="root:root@tcp(127.0.0.1:${MYSQL80_PORT})/${MYSQL80_DB}?parseTime=true"
echo "[task-desired] MySQL 8 schema-4 seed then 000005 and 000006"
compose_mysql mysql80 -e "DROP DATABASE IF EXISTS ${MYSQL80_DB}; CREATE DATABASE ${MYSQL80_DB} DEFAULT CHARACTER SET utf8mb4;"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$MYSQL80_DSN" go run ./cmd/migrate goto 4
)
seed_binlog_segments mysql80 "$MYSQL80_DB"
mysql80_before="$(service_one mysql80 "$MYSQL80_DB" "SELECT COUNT(*) FROM binlog_files")"
mysql80_positions="$(segment_positions mysql80 "$MYSQL80_DB")"
if [[ "$mysql80_before" != "4" || "$mysql80_positions" != "$positions_before" ]]; then
  printf 'mysql80 positions:\n%s\n' "$mysql80_positions" >&2
  fail "mysql80 seed count=$mysql80_before"
fi
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$MYSQL80_DSN" go run ./cmd/migrate goto 5
)
assert_schema5_catalog mysql80 mysql80 "$MYSQL80_DB" "$mysql80_before" "$mysql80_positions"

echo "[task-desired] v0.5.49 still starts on schema 5"
V0549_BIN="$(download_release 0.5.49)"
V0549_LOG="${SERVER_LOG}.v0549"
V0549_DATA="${DATA_DIR}-v0549"
mkdir -p "$V0549_DATA"
: >"$V0549_LOG"
BINLOG_SERVER_DATA_DIR="$V0549_DATA" \
BINLOG_SERVER_META_DSN="$MYSQL80_DSN" \
BINLOG_SERVER_MODE=cluster \
BINLOG_SERVER_CLUSTER_ROLE=control-plane \
  nohup "$V0549_BIN" --config "$ROOT_DIR/deploy/e2e/config.yaml" >"$V0549_LOG" 2>&1 &
SERVER_PID=$!
wait_healthz "$V0549_LOG" "v0.5.49 schema 5"
kill_server

echo "[task-desired] migrate up 000006 on MySQL 8"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$MYSQL80_DSN" go run ./cmd/migrate up
)
assert_schema6_catalog mysql80 mysql80 "$MYSQL80_DB" "$mysql80_before"

echo "[task-desired] seal, enroll, and upload epoch 0 and epoch 1 on schema 6"
(
  cd "$ROOT_DIR"
  BINLOG_TEST_META_DSN="$MYSQL80_DSN" go test -count=1 -timeout 300s -run 'TestIssue189_SealedEpochsOnMySQL$' ./internal/replication/
)

echo "[task-desired] current binary on MySQL 8 schema 6 lists both epochs"
MYSQL80_DATA="${DATA_DIR}-mysql80"
MYSQL80_LOG="${SERVER_LOG}.mysql80"
start_current "$MYSQL80_DSN" "$MYSQL80_DATA" "$MYSQL80_LOG"
wait_healthz "$MYSQL80_LOG" "mysql80 schema 6"
files_json="$(curl -fsS "$API/api/tasks/task-1/files")"
printf '%s\n' "$files_json" | jq -e '
  ([.[] | select(.file_name=="mysql-bin.000009")] | length) == 2
  and ([.[] | select(.file_name=="mysql-bin.000009" and ((.epoch // 0) == 0) and .start_pos == 4 and .end_pos > 0 and .state == "SEALED")] | length) == 1
  and ([.[] | select(.file_name=="mysql-bin.000009" and .epoch == 1 and .start_pos == 4 and .end_pos > 0 and .state == "SEALED")] | length) == 1
  and ([.[] | select(.file_name=="mysql-bin.000009" and .upload_state == "UPLOADED")] | length) == 2
  and (([.[] | select(.file_name=="mysql-bin.000009") | .end_pos] | unique | length) == 2)
' >/dev/null
printf '%s\n' "$files_json" | jq -r '.[] | select(.file_name=="mysql-bin.000009") | "\(.epoch // 0) \(.start_pos) \(.end_pos) \(.state)"'
kill_server

echo "[task-desired] v0.5.52 refuses schema 6 with missing index uk_task_file_epoch"
V0552_BIN="$(download_release 0.5.52)"
expect_missing_file_epoch "$V0552_BIN" "$MYSQL80_DSN" "${DATA_DIR}-v0552" "${SERVER_LOG}.v0552" "v0.5.52 schema 6"

mysql80_before_down006="$(service_one mysql80 "$MYSQL80_DB" "SELECT COUNT(*) FROM binlog_files")"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$MYSQL80_DSN" go run ./cmd/migrate down --steps 1
)
assert_file_epoch_restored mysql80 mysql80 "$MYSQL80_DB" "$mysql80_before_down006"

mysql80_before_down="$(service_one mysql80 "$MYSQL80_DB" "SELECT COUNT(*) FROM binlog_files")"
(
  cd "$ROOT_DIR"
  MIGRATE_ENV=dev META_DSN="$MYSQL80_DSN" go run ./cmd/migrate down --steps 1
)
assert_schema4_after_down mysql80 mysql80 "$MYSQL80_DB" "$mysql80_before_down"

echo "[task-desired] current binary must refuse MySQL 8 schema 4"
expect_refuse "$MYSQL80_DSN" "${DATA_DIR}-mysql80-schema4" "${SERVER_LOG}.mysql80.schema4" "mysql80 schema 4"

compose_mysql mysql80 -e "DROP DATABASE IF EXISTS ${MYSQL80_DB};" >/dev/null
echo "[task-desired] success: 000006 drops uk_task_file_epoch on Percona 5.7 and MySQL 8, schema 6 healthz, seal and files API, v0.5.49 healthz on schema 5, v0.5.52 missing index on schema 6, down keeps rows"
