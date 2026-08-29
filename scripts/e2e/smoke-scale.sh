#!/usr/bin/env bash
# input: canonical E2E topology, batch/pagination APIs, local capacity, and the suite server process
# output: opt-in full-live-stream progress assertions and a machine-readable capacity/performance report
# pos: E2E scale evidence for control-plane pagination and bounded live replication streams
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"

API="${E2E_API:-http://127.0.0.1:18080}"
CONTROL_TASKS="${E2E_SCALE_CONTROL_TASKS:-1000}"
LIVE_STREAMS="${E2E_SCALE_LIVE_STREAMS:-100}"
START_RATE="${E2E_SCALE_START_RATE:-10}"
REQUEST_DELAY="${E2E_SCALE_REQUEST_DELAY_SEC:-0.03}"
DATA_DIR="${E2E_DATA_DIR:-$ROOT_DIR/tmp/e2e/data-scale-$(date +%s)}"
SERVER_PID="${E2E_SERVER_PID:-}"
SERVER_LOG="${E2E_SERVER_LOG:-/tmp/binlog-server-e2e-suite.log}"
RUN_TAG="$(date +%s)"
REPORT="${E2E_SCALE_REPORT:-$ROOT_DIR/tmp/e2e/scale-report-${RUN_TAG}.json}"
TASK_IDS="$DATA_DIR/scale-task-ids.txt"
CHECKPOINTS="$DATA_DIR/scale-checkpoints.tsv"
PROGRESS_MISSING="$DATA_DIR/scale-progress-missing.txt"
DATA_PATHS="$DATA_DIR/scale-data-paths.txt"

fail() { echo "[scale] $*" >&2; exit 1; }
need_cmd() { command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"; }
is_uint() { [[ "$1" =~ ^[0-9]+$ ]]; }

for cmd in curl jq docker awk sort df ps; do need_cmd "$cmd"; done
is_uint "$CONTROL_TASKS" && (( CONTROL_TASKS >= 1000 && CONTROL_TASKS % 100 == 0 )) || fail "E2E_SCALE_CONTROL_TASKS must be a multiple of 100 and at least 1000"
is_uint "$LIVE_STREAMS" && (( LIVE_STREAMS >= 1 && LIVE_STREAMS <= 300 )) || fail "E2E_SCALE_LIVE_STREAMS must be 1..300"
is_uint "$START_RATE" && (( START_RATE >= 1 && START_RATE <= 25 )) || fail "E2E_SCALE_START_RATE must be 1..25"
(( LIVE_STREAMS <= CONTROL_TASKS )) || fail "live streams cannot exceed control tasks"

mkdir -p "$DATA_DIR" "$(dirname "$REPORT")"
touch "$TASK_IDS"
LOG_BYTES_BEFORE="$(wc -c <"$SERVER_LOG" 2>/dev/null || echo 0)"

preflight() {
  local fd min_fd disk_kb
  [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" >/dev/null 2>&1 || fail "E2E_SERVER_PID must name a live suite server before scale evidence runs"
  fd="$(ulimit -n)"
  min_fd=$((LIVE_STREAMS * 4 + 256))
  is_uint "$fd" && (( fd >= min_fd )) || fail "fd capacity too low: have=$fd need=$min_fd (raise ulimit -n)"
  disk_kb="$(df -Pk "$DATA_DIR" | awk 'NR==2 {print $4}')"
  is_uint "$disk_kb" && (( disk_kb >= ${E2E_SCALE_MIN_DISK_MB:-1024} * 1024 )) || fail "disk capacity too low: have=${disk_kb:-unknown}KB need=${E2E_SCALE_MIN_DISK_MB:-1024}MB"
  ensure_max_connections mysql57
  SOURCE_MAX_CONNECTIONS="$MAX_CONNECTIONS"
  ensure_max_connections meta-primary
  META_MAX_CONNECTIONS="$MAX_CONNECTIONS"
  PREFLIGHT_FD="$fd" PREFLIGHT_DISK_KB="$disk_kb"
}

ensure_max_connections() {
  local service="$1" desired=$((LIVE_STREAMS + 20))
  docker compose -f "$COMPOSE_FILE" exec -T "$service" mysql -uroot -proot -e "SET GLOBAL max_connections = $desired" >/dev/null
  MAX_CONNECTIONS="$(docker compose -f "$COMPOSE_FILE" exec -T "$service" mysql -N -uroot -proot -e "SHOW VARIABLES LIKE 'max_connections'" | awk '{print $2}')"
  [[ "$MAX_CONNECTIONS" == "$desired" ]] || fail "$service max_connections not honored: have=${MAX_CONNECTIONS:-unknown} need=$desired"
}

api() {
  local method="$1" path="$2" data="${3:-}" raw
  if [[ -n "$data" ]]; then
    raw="$(curl -sS -w $'\n%{http_code}' -X "$method" "$API$path" -H 'Content-Type: application/json' --data "$data")" || fail "request failed: $method $path"
  else
    raw="$(curl -sS -w $'\n%{http_code}' -X "$method" "$API$path")" || fail "request failed: $method $path"
  fi
  API_STATUS="${raw##*$'\n'}"
  API_BODY="${raw%$'\n'*}"
  [[ "$API_STATUS" != "429" && "$API_STATUS" != 5* ]] || fail "unexpected HTTP $API_STATUS: $method $path body=$API_BODY"
}

create_batch() {
  local first="$1" count="$2" payload
  payload="$(jq -n --arg tag "$RUN_TAG" --arg host "$E2E_SOURCE_HOST" --arg user "$E2E_SOURCE_USER" --arg pass "$E2E_SOURCE_PASS" --argjson port "$E2E_MYSQL57_PORT" --argjson first "$first" --argjson count "$count" '
    {items: [range($first; $first + $count) | {
      name: ("e2e-scale-" + $tag + "-" + tostring),
      cluster_key: ("e2e-scale-" + $tag + "-" + tostring),
      source: {host:$host, port:$port, user:$user, password:$pass, flavor:"mysql", server_id:(610000 + .)},
      start: {mode:"LATEST"}, storage:{retention_days:7}
    }]}')"
  api POST /api/tasks/batch "$payload"
  [[ "$API_STATUS" == "200" ]] || fail "batch create returned $API_STATUS: $API_BODY"
  printf '%s' "$API_BODY" | jq -e --argjson expected "$count" '[.[] | select(.task != null)] | length == $expected and ([.[] | select(.error != null)] | length == 0)' >/dev/null || fail "batch did not fully succeed"
  printf '%s' "$API_BODY" | jq -r '.[].task.id' >>"$TASK_IDS"
}

verify_pagination() {
  local offset body
  : >"$DATA_DIR/reassembled-ids.txt"
  for ((offset=0; offset<CONTROL_TASKS; offset+=100)); do
    api GET "/api/tasks?limit=100&offset=$offset"
    [[ "$API_STATUS" == "200" ]] || fail "task pagination returned $API_STATUS"
    printf '%s' "$API_BODY" | jq -e --argjson total "$CONTROL_TASKS" '.total == $total and .limit == 100 and (.items | length) <= 100' >/dev/null || fail "bad task page metadata at offset=$offset"
    printf '%s' "$API_BODY" | jq -r '.items[].id' >>"$DATA_DIR/reassembled-ids.txt"
  done
  [[ "$(sort "$TASK_IDS" | uniq | wc -l | tr -d ' ')" == "$CONTROL_TASKS" ]] || fail "created task ids are not unique"
  [[ "$(sort "$DATA_DIR/reassembled-ids.txt" | uniq | wc -l | tr -d ' ')" == "$CONTROL_TASKS" ]] || fail "pagination has gaps or duplicates"
  cmp <(sort "$TASK_IDS") <(sort "$DATA_DIR/reassembled-ids.txt") || fail "pagination did not reassemble all task ids"

  api GET "/api/dashboard?limit=100&offset=0"
  [[ "$API_STATUS" == "200" ]] || fail "dashboard returned $API_STATUS"
  printf '%s' "$API_BODY" | jq -e --argjson total "$CONTROL_TASKS" '
    .total == $total and .summary.total == $total and (.tasks | length) == 100 and
    ([.sources[].task_count] | add) == $total' >/dev/null || fail "dashboard aggregation or detail pagination is incomplete"
}

write_source_marker() {
  local marker="$1"
  docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -uroot -proot -e "INSERT INTO binlog_e2e_57.t1(v) VALUES('${marker}');"
}

start_live_streams() {
  local i id state
  LIVE_IDS=()
  while IFS= read -r id; do LIVE_IDS+=("$id"); done < <(head -n "$LIVE_STREAMS" "$TASK_IDS")
  for ((i=0; i<LIVE_STREAMS; i++)); do
    id="${LIVE_IDS[$i]}"
    api POST "/api/tasks/$id/start"
    [[ "$API_STATUS" == "204" ]] || fail "start failed for $id: $API_STATUS $API_BODY"
    sleep "$(awk -v rate="$START_RATE" 'BEGIN { printf "%.3f", 1/rate }')"
  done
  for _ in {1..180}; do
    api GET /api/summary
    if printf '%s' "$API_BODY" | jq -e --argjson live "$LIVE_STREAMS" '.running == $live and .failed == 0 and .retry_backoff == 0' >/dev/null; then return; fi
    sleep 1
  done
  fail "live streams did not converge: $API_BODY"
}

snapshot_checkpoints() {
  local id file pos
  : >"$CHECKPOINTS"
  for id in "${LIVE_IDS[@]}"; do
    for _ in {1..120}; do
      api GET "/api/tasks/$id/checkpoint"
      file="$(printf '%s' "$API_BODY" | jq -r '.file // ""')"; pos="$(printf '%s' "$API_BODY" | jq -r '.pos // 0')"
      [[ "$API_STATUS" == "200" && -n "$file" && "$pos" =~ ^[0-9]+$ ]] && break
      sleep 1
    done
    [[ "$API_STATUS" == "200" && -n "$file" && "$pos" =~ ^[0-9]+$ ]] || fail "checkpoint not ready for $id: $API_BODY"
    printf '%s\t%s\t%s\n' "$id" "$file" "$pos" >>"$CHECKPOINTS"
    sleep "$REQUEST_DELAY"
  done
}

verify_all_progress() {
  local id before_file before_pos file pos advanced=0
  : >"$PROGRESS_MISSING"; : >"$DATA_PATHS"
  write_source_marker "scale-progress-${RUN_TAG}"
  while IFS=$'\t' read -r id before_file before_pos; do
    for _ in {1..180}; do
      api GET "/api/tasks/$id/checkpoint"
      file="$(printf '%s' "$API_BODY" | jq -r '.file // ""')"; pos="$(printf '%s' "$API_BODY" | jq -r '.pos // 0')"
      if [[ "$file" != "$before_file" || ( "$pos" =~ ^[0-9]+$ && "$pos" -gt "$before_pos" ) ]]; then break; fi
      sleep "$REQUEST_DELAY"
    done
    if [[ "$file" == "$before_file" && ( ! "$pos" =~ ^[0-9]+$ || "$pos" -le "$before_pos" ) ]]; then
      printf '%s\n' "$id" >>"$PROGRESS_MISSING"
      continue
    fi
    [[ -s "$DATA_DIR/$id/$file" ]] || { printf '%s\n' "$id" >>"$PROGRESS_MISSING"; continue; }
    printf '%s\t%s\n' "$id" "$DATA_DIR/$id/$file" >>"$DATA_PATHS"
    advanced=$((advanced + 1))
    sleep "$REQUEST_DELAY"
  done <"$CHECKPOINTS"
  PROGRESS_ADVANCED="$advanced"
  PROGRESS_MISSING_COUNT="$(wc -l <"$PROGRESS_MISSING" | tr -d ' ')"
  [[ "$PROGRESS_MISSING_COUNT" == "0" ]] || fail "not every live stream advanced or wrote an open file: $(tr '\n' ' ' <"$PROGRESS_MISSING")"
  [[ "$(cut -f1 "$DATA_PATHS" | sort -u | wc -l | tr -d ' ')" == "$LIVE_STREAMS" ]] || fail "live task IDs are not uniquely evidenced"
  [[ "$(cut -f2 "$DATA_PATHS" | sort -u | wc -l | tr -d ' ')" == "$LIVE_STREAMS" ]] || fail "live task data paths are not unique"
}

verify_unreachable() {
  local bad_payload bad_id state err
  bad_payload="$(jq -n --arg tag "$RUN_TAG" '{name:("e2e-scale-unreachable-"+$tag),cluster_key:("e2e-scale-unreachable-"+$tag),source:{host:"127.0.0.1",port:1,user:"repl",password:"replpass",flavor:"mysql",server_id:999999},start:{mode:"LATEST"},storage:{retention_days:7}}')"
  api POST /api/tasks "$bad_payload"; [[ "$API_STATUS" == "201" ]] || fail "unreachable task create failed"
  bad_id="$(printf '%s' "$API_BODY" | jq -r '.id')"
  api POST "/api/tasks/$bad_id/start"; [[ "$API_STATUS" == "204" ]] || fail "unreachable task start failed"
  for _ in {1..300}; do
    api GET "/api/tasks/$bad_id"
    state="$(printf '%s' "$API_BODY" | jq -r '.state // empty')"
    err="$(printf '%s' "$API_BODY" | jq -r '.last_error // empty')"
    [[ "$state" == "FAILED" && "$err" == *SOURCE_UNREACHABLE* ]] && return
    sleep 1
  done
  fail "unreachable source did not converge to FAILED/SOURCE_UNREACHABLE (state=$state error=$err)"
}

percentiles() {
  local path="$1" samples="$DATA_DIR/latency-$(echo "$path" | tr '/?&=' '_').txt" sample status latency n p50 p95
  : >"$samples"
  for _ in {1..20}; do
    sample="$(curl -sS -o /dev/null -w '%{http_code} %{time_total}' "$API$path")" || fail "latency sample failed: $path"
    status="${sample%% *}"; latency="${sample#* }"
    [[ "$status" != "429" && "$status" != 5* && "$status" == 2* ]] || fail "unexpected HTTP $status while sampling $path"
    printf '%s\n' "$latency" >>"$samples"
  done
  sort -n "$samples" -o "$samples"; n="$(wc -l <"$samples" | tr -d ' ')"
  p50="$(awk -v n="$n" 'NR == int((n+1)*.50) {print; exit}' "$samples")"
  p95="$(awk -v n="$n" 'NR == int((n+1)*.95) {print; exit}' "$samples")"
  printf '%s %s' "$p50" "$p95"
}

preflight
echo "[scale] create $CONTROL_TASKS control tasks in batches of 100"
for ((start=0; start<CONTROL_TASKS; start+=100)); do create_batch "$start" 100; done
verify_pagination
start_live_streams
write_source_marker "scale-prime-${RUN_TAG}"
snapshot_checkpoints
verify_all_progress
verify_unreachable

read -r API_P50 API_P95 <<<"$(percentiles '/api/tasks?limit=100&offset=0')"
read -r DASHBOARD_P50 DASHBOARD_P95 <<<"$(percentiles '/api/dashboard?limit=100&offset=0')"
read -r METRICS_P50 METRICS_P95 <<<"$(percentiles '/metrics')"
RSS_KB="$(ps -o rss= -p "$SERVER_PID" 2>/dev/null | tr -d ' ' || true)"
LOG_BYTES_AFTER="$(wc -c <"$SERVER_LOG" 2>/dev/null || echo "$LOG_BYTES_BEFORE")"
SOURCE_THREADS="$(docker compose -f "$COMPOSE_FILE" exec -T mysql57 mysql -N -uroot -proot -e "SHOW STATUS LIKE 'Threads_connected'" | awk '{print $2}')"
META_THREADS="$(docker compose -f "$COMPOSE_FILE" exec -T meta-primary mysql -N -uroot -proot -e "SHOW STATUS LIKE 'Threads_connected'" | awk '{print $2}')"

jq -n \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg api "$API" \
  --argjson control_tasks "$CONTROL_TASKS" --argjson live_streams "$LIVE_STREAMS" --argjson start_rate "$START_RATE" \
  --argjson fd_limit "$PREFLIGHT_FD" --argjson disk_available_kb "$PREFLIGHT_DISK_KB" --argjson source_max_connections "$SOURCE_MAX_CONNECTIONS" --argjson meta_max_connections "$META_MAX_CONNECTIONS" \
  --argjson source_threads_connected "${SOURCE_THREADS:-0}" --argjson meta_threads_connected "${META_THREADS:-0}" --argjson process_rss_kb "${RSS_KB:-0}" \
  --argjson log_delta_bytes "$((LOG_BYTES_AFTER - LOG_BYTES_BEFORE))" \
  --argjson api_p50 "$API_P50" --argjson api_p95 "$API_P95" --argjson dashboard_p50 "$DASHBOARD_P50" --argjson dashboard_p95 "$DASHBOARD_P95" --argjson metrics_p50 "$METRICS_P50" --argjson metrics_p95 "$METRICS_P95" --argjson progress_expected "$LIVE_STREAMS" --argjson progress_advanced "$PROGRESS_ADVANCED" --argjson progress_missing "$PROGRESS_MISSING_COUNT" --argjson unique_ids "$(cut -f1 "$DATA_PATHS" | sort -u | wc -l | tr -d ' ')" --argjson unique_data_paths "$(cut -f2 "$DATA_PATHS" | sort -u | wc -l | tr -d ' ')" --rawfile data_path_evidence "$DATA_PATHS" \
  '{generated_at:$generated_at,api:$api,control_tasks:$control_tasks,live_streams:$live_streams,start_rate_per_second:$start_rate,preflight:{fd_limit:$fd_limit,disk_available_kb:$disk_available_kb,source_max_connections:$source_max_connections,meta_max_connections:$meta_max_connections},capacity:{source_threads_connected:$source_threads_connected,meta_threads_connected:$meta_threads_connected,process_rss_kb:$process_rss_kb,server_log_delta_bytes:$log_delta_bytes},progress:{expected:$progress_expected,advanced:$progress_advanced,missing:$progress_missing,unique_task_ids:$unique_ids,unique_data_paths:$unique_data_paths,evidence:($data_path_evidence | split("\n") | map(select(length > 0) | split("\t") | {task_id:.[0],data_path:.[1]}))},latency_seconds:{api:{p50:$api_p50,p95:$api_p95},dashboard:{p50:$dashboard_p50,p95:$dashboard_p95},metrics:{p50:$metrics_p50,p95:$metrics_p95}},notes:["One MySQL fixture supplies many clients; this does not model independent source clusters.","No performance SLA is asserted."]}' >"$REPORT"
echo "[scale] PASS report=$REPORT"
