#!/usr/bin/env bash
# input: mysql80 with GTID on, the suite API, the task data directory, and the Percona 8.0 mysqlbinlog client
# output: coverage answers for 10 inside, 10 outside, and 10 boundary cases of both time and GTID that match an independent mysqlbinlog parse; five point-in-time restores whose CHECKSUM TABLE matches the source; a red full-backup precheck for a hole and for an overlap
# pos: CI proof for ADR 0009 phases 4 and 5 (production-readiness D3)
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
source "$ROOT_DIR/scripts/e2e/lib-topology.sh"
API="${E2E_API:-http://127.0.0.1:18080}"
RUN_TAG="$(date +%s)"
DB="binlog_cov_${RUN_TAG}"
BASE=1704067200
RESTORE="binlog-e2e-coverage"
WORKDIR="$(mktemp -d)"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq
need_cmd awk
need_cmd date

fail() {
  echo "[coverage] $*" >&2
  exit 1
}

mysql80() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot --batch --raw -Nse "$1" | tr -d '\r'
}

restore_compression() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -e "SET GLOBAL binlog_transaction_compression=ON;" >/dev/null 2>&1 || true
}

cleanup() {
  docker rm -f -v "$RESTORE" >/dev/null 2>&1 || true
  restore_compression
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

abs_path() {
  local p="$1"
  if [[ "$p" == /* ]]; then
    printf '%s' "$p"
  else
    printf '%s' "$ROOT_DIR/${p#./}"
  fi
}

gtid_subset() {
  local inner="$1"
  local outer="$2"
  inner="$(printf '%s' "$inner" | tr -d ' \r\n')"
  outer="$(printf '%s' "$outer" | tr -d ' \r\n')"
  mysql80 "SELECT GTID_SUBSET('${inner}', '${outer}');"
}

task_json() {
  curl -fsS "$API/api/tasks/$1"
}

wait_state() {
  local id="$1"
  local want="$2"
  local i st
  for i in $(seq 1 60); do
    st="$(task_json "$id" | jq -r '.state // empty')"
    if [[ "$st" == "$want" ]]; then
      return 0
    fi
    sleep 0.5
  done
  fail "task $id did not reach $want: $(task_json "$id")"
}

wait_gtid() {
  local want="$1"
  local i gtid
  for i in $(seq 1 90); do
    gtid="$(curl -fsS "$API/api/tasks/$task_id/checkpoint" | jq -r '.gtid_set // empty')"
    if [[ -n "$gtid" && "$(gtid_subset "$want" "$gtid")" == "1" ]]; then
      return 0
    fi
    sleep 0.5
  done
  fail "checkpoint does not include $want: $(curl -fsS "$API/api/tasks/$task_id/checkpoint")"
}

wait_sealed_count() {
  local want="$1"
  local i n
  for i in $(seq 1 60); do
    n="$(curl -fsS "$API/api/tasks/$task_id/files?limit=50" | jq '[.[] | select(.state == "SEALED")] | length')"
    if [[ "$n" -ge "$want" ]]; then
      return 0
    fi
    sleep 0.5
  done
  fail "expected at least $want sealed files: $(curl -fsS "$API/api/tasks/$task_id/files?limit=50")"
}

clock_shift() {
  date -u -d "$1 UTC $2" '+%Y-%m-%d %H:%M:%S'
}

checksum_source() {
  mysql80 "CHECKSUM TABLE ${DB}.t" | awk '{ print $2 }'
}

start_restore() {
  docker rm -f -v "$RESTORE" >/dev/null 2>&1 || true
  docker run -d --name "$RESTORE" \
    -e MYSQL_ROOT_PASSWORD=root \
    -e MYSQL_ROOT_HOST=% \
    mysql:8.0 \
    --gtid-mode=ON \
    --enforce-gtid-consistency=ON \
    --server-id=310183 \
    --log-bin=mysql-bin \
    --binlog-format=ROW \
    --default-time-zone=+00:00 >/dev/null
  local i got errf
  errf="$(mktemp)"
  for i in $(seq 1 90); do
    if docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "SELECT 1" >/dev/null 2>"$errf"; then
      if docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "RESET MASTER;" >/dev/null 2>"$errf"; then
        got="$(docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "SELECT @@GLOBAL.gtid_executed = ''" 2>>"$errf" | tr -d '[:space:]' || true)"
        if [[ "$got" == "1" ]]; then
          rm -f "$errf"
          return 0
        fi
      fi
    fi
    sleep 1
  done
  echo "restore could not reset" >&2
  cat "$errf" >&2 || true
  rm -f "$errf"
  docker logs "$RESTORE" >&2 || true
  return 1
}

reset_restore_schema() {
  docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -e "
    RESET MASTER;
    DROP DATABASE IF EXISTS ${DB};
    CREATE DATABASE ${DB};
    CREATE TABLE ${DB}.t (id INT PRIMARY KEY, v VARCHAR(64) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
  " >/dev/null
}

parse_file() {
  local path="$1"
  local base size dest dump
  base="$(basename "$path")"
  size="$(wc -c <"$path" | tr -d ' ')"
  dest="/tmp/coverage-oracle-${base}"
  docker compose -f "$COMPOSE_FILE" cp "$path" "percona80:${dest}" </dev/null
  dump="$(docker compose -f "$COMPOSE_FILE" exec -T -e TZ=UTC percona80 mysqlbinlog -v "$dest" </dev/null | tr -d '\r')"
  printf '%s\n' "$dump" | awk -v base="$base" -v size="$size" '
    function pad(tm,    a) {
      split(tm, a, ".")
      tm = a[1]
      split(tm, a, ":")
      if (length(a[1]) == 1) a[1] = "0" a[1]
      return a[1] ":" a[2] ":" a[3]
    }
    function emit(endpos) {
      if (off == "") return
      c = header ? 0 : 1
      printf "%s\t%s\t%s\t%s\t%s\t%s\n", base, off, endpos, ts, gtid, c
    }
    /^# at [0-9]+$/ {
      if (off != "") emit($3)
      off = $3
      ts = ""
      header = 0
      gtid = ""
      next
    }
    /^#[0-9]{6} / && ts == "" {
      ymd = substr($1, 2)
      ts = sprintf("20%s-%s-%s %s", substr(ymd, 1, 2), substr(ymd, 3, 2), substr(ymd, 5, 2), pad($2))
    }
    /Start:/ || /Previous-GTIDs/ { header = 1 }
    /GTID_NEXT=/ {
      line = $0
      sub(/.*GTID_NEXT=[[:space:]]*\047/, "", line)
      sub(/\047.*/, "", line)
      gtid = tolower(line)
    }
    END { if (off != "") emit(size) }
  '
}

echo "[coverage] compression off"
mysql80 "SET GLOBAL binlog_transaction_compression=OFF;"
mysql80 "CREATE DATABASE ${DB}; CREATE TABLE ${DB}.t (id INT PRIMARY KEY, v VARCHAR(64) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;"
# Five statements before the task so five earlier GTIDs sit outside the retained window.
mysql80 "CREATE TABLE ${DB}.pre1 (id INT); CREATE TABLE ${DB}.pre2 (id INT); CREATE TABLE ${DB}.pre3 (id INT); CREATE TABLE ${DB}.pre4 (id INT); CREATE TABLE ${DB}.pre5 (id INT);"
before="$(mysql80 "SELECT @@GLOBAL.gtid_executed;" | tr -d ' \n')"
echo "[coverage] start set $before"

name="e2e-coverage-${RUN_TAG}"
body="$(jq -n \
  --arg name "$name" \
  --arg gtid "$before" \
  --arg host "$E2E_SOURCE_HOST" \
  --arg user "$E2E_SOURCE_USER" \
  --arg pass "$E2E_SOURCE_PASS" \
  --argjson port "$E2E_MYSQL80_PORT" \
  '{name:$name,cluster_key:$name,source:{host:$host,port:$port,user:$user,password:$pass,flavor:"mysql",server_id:310184},start:{mode:"GTID",gtid_set:$gtid},storage:{retention_days:7}}')"
resp="$(curl -sS -X POST "$API/api/tasks" -H 'Content-Type: application/json' -d "$body")"
task_id="$(printf '%s' "$resp" | jq -r '.id // empty')"
[[ -n "$task_id" && "$task_id" != "null" ]] || fail "create task failed: $resp"
curl -fsS -X POST "$API/api/tasks/$task_id/start" >/dev/null
wait_state "$task_id" "RUNNING"
wait_gtid "$before"

declare -a POINT_SUMS=()
declare -a POINT_AT=()
n=0
for seg in 1 2 3 4 5; do
  for j in 1 2 3 4; do
    n=$((n + 1))
    ts=$((BASE + (n - 1) * 10))
    mysql80 "SET TIMESTAMP=${ts}; INSERT INTO ${DB}.t VALUES (${n}, 'v-${n}');"
    executed="$(mysql80 "SELECT @@GLOBAL.gtid_executed;" | tr -d ' \n')"
    wait_gtid "$executed"
  done
  POINT_AT+=("$(date -u -d "@${ts}" '+%Y-%m-%d %H:%M:%S')")
  POINT_SUMS+=("$(checksum_source)")
  echo "[coverage] segment $seg at ${POINT_AT[$((seg - 1))]} checksum ${POINT_SUMS[$((seg - 1))]}"
  # Flush every segment, including the last. Stop alone leaves that file OPEN.
  mysql80 "FLUSH BINARY LOGS;"
  wait_sealed_count "$seg"
  if [[ "$seg" -lt 5 ]]; then
    # The rotate event is stamped with the source clock. A one-second gap keeps
    # each sealed segment's last event on its own second.
    sleep 2
  fi
done
curl -fsS -X POST "$API/api/tasks/$task_id/stop" >/dev/null
wait_state "$task_id" "STOPPED"

mapfile -t PATHS < <(curl -fsS "$API/api/tasks/$task_id/files?limit=50" | jq -r 'sort_by(.file_name) | .[] | select(.state == "SEALED") | .file_path')
[[ "${#PATHS[@]}" -ge 5 ]] || fail "need 5 sealed files, got ${#PATHS[@]}: $(curl -fsS "$API/api/tasks/$task_id/files?limit=50")"

: >"$WORKDIR/events.tsv"
for p in "${PATHS[@]}"; do
  abs="$(abs_path "$p")"
  [[ -f "$abs" ]] || fail "missing $abs"
  parse_file "$abs" >>"$WORKDIR/events.tsv"
done
[[ -s "$WORKDIR/events.tsv" ]] || fail "mysqlbinlog produced no events"

DATA_FILES=()
for p in "${PATHS[@]}"; do
  base="$(basename "$p")"
  distinct="$(awk -F '\t' -v b="$base" '$1 == b && $6 == 1 && $4 != "" { print $4 }' "$WORKDIR/events.tsv" | sort -u | wc -l | tr -d ' ')"
  if [[ "$distinct" -ge 4 ]]; then
    DATA_FILES+=("$base")
  fi
done
[[ "${#DATA_FILES[@]}" -ge 5 ]] || fail "need 5 files with 4 timestamps, got ${#DATA_FILES[@]} (${DATA_FILES[*]:-none}). events: $(wc -l <"$WORKDIR/events.tsv")"
DATA_FILES=("${DATA_FILES[@]:0:5}")

INSIDE_AT=()
BOUNDARY_AT=()
for base in "${DATA_FILES[@]}"; do
  mapfile -t stamps < <(awk -F '\t' -v b="$base" '$1 == b && $6 == 1 && $4 != "" { print $4 }' "$WORKDIR/events.tsv" | sort -u)
  [[ "${#stamps[@]}" -ge 4 ]] || fail "$base has ${#stamps[@]} timestamps"
  BOUNDARY_AT+=("${stamps[0]}" "${stamps[${#stamps[@]}-1]}")
  INSIDE_AT+=("${stamps[1]}" "${stamps[2]}")
done
[[ "${#INSIDE_AT[@]}" -eq 10 && "${#BOUNDARY_AT[@]}" -eq 10 ]] || fail "time case count inside=${#INSIDE_AT[@]} boundary=${#BOUNDARY_AT[@]}"
[[ "$(printf '%s\n' "${INSIDE_AT[@]}" | sort -u | wc -l | tr -d ' ')" == "10" ]] || fail "inside times are not 10 distinct values: ${INSIDE_AT[*]}"
[[ "$(printf '%s\n' "${BOUNDARY_AT[@]}" | sort -u | wc -l | tr -d ' ')" == "10" ]] || fail "boundary times are not 10 distinct values: ${BOUNDARY_AT[*]}"

earliest="$(awk -F '\t' '$6 == 1 && $4 != "" { print $4 }' "$WORKDIR/events.tsv" | sort | head -1)"
latest="$(awk -F '\t' '$6 == 1 && $4 != "" { print $4 }' "$WORKDIR/events.tsv" | sort | tail -1)"
OUTSIDE_AT=()
for shift in "- 1 second" "- 2 seconds" "- 10 seconds" "- 60 seconds" "- 3600 seconds"; do
  OUTSIDE_AT+=("$(clock_shift "$earliest" "$shift")")
done
for shift in "+ 1 second" "+ 2 seconds" "+ 10 seconds" "+ 60 seconds" "+ 3600 seconds"; do
  OUTSIDE_AT+=("$(clock_shift "$latest" "$shift")")
done
[[ "${#OUTSIDE_AT[@]}" -eq 10 ]] || fail "outside time count"
[[ "$(printf '%s\n' "${OUTSIDE_AT[@]}" | sort -u | wc -l | tr -d ' ')" == "10" ]] || fail "outside times are not 10 distinct values: ${OUTSIDE_AT[*]}"

# Copied-event stop: end of the last copied event at or before at, else uncovered.
oracle_time() {
  local at="$1"
  awk -F '\t' -v at="$at" '
    $6 == 1 && $4 != "" && $4 <= at { file=$1; pos=$3; names[$1]=1; seen=1 }
    $6 == 1 && $4 != "" { if (earliest == "" || $4 < earliest) earliest=$4; if ($4 > latest) latest=$4 }
    END {
      if (!seen) {
        if (at < earliest) print "0\tbefore earliest"
        else print "0\tafter latest"
        exit
      }
      printf "1\t%s\t%s", file, pos
      # prefix is filled by the caller from the chain
      printf "\n"
    }
  ' "$WORKDIR/events.tsv"
}

chain_prefix() {
  local stop="$1"
  local name out=""
  for p in "${PATHS[@]}"; do
    name="$(basename "$p")"
    out+="${out:+,}$name"
    if [[ "$name" == "$stop" ]]; then
      printf '%s' "$out"
      return 0
    fi
  done
  return 1
}

assert_time() {
  local kind="$1"
  local at="$2"
  local json got covered reason seg pos names want
  json="$(curl -fsS -G "$API/api/tasks/$task_id/coverage" --data-urlencode "at=$at" --data-urlencode "limit=1")"
  covered="$(jq -r '.covered' <<<"$json")"
  reason="$(jq -r '.reason // empty' <<<"$json")"
  want="$(oracle_time "$at")"
  local want_covered want_file want_pos
  want_covered="${want%%$'\t'*}"
  if [[ "$covered" != "true" && "$want_covered" == "1" ]] || [[ "$covered" == "true" && "$want_covered" == "0" ]]; then
    fail "$kind at=$at covered=$covered oracle=$want body=$json"
  fi
  if [[ "$want_covered" == "0" ]]; then
    local want_reason="${want#*$'\t'}"
    [[ "$covered" == "false" && "$reason" == "$want_reason" ]] || fail "$kind at=$at reason=$reason want=$want_reason"
    [[ "$(jq '.segments | length' <<<"$json")" == "0" ]] || fail "$kind at=$at segments not empty"
    return 0
  fi
  want_file="$(printf '%s' "$want" | awk -F '\t' '{ print $2 }')"
  want_pos="$(printf '%s' "$want" | awk -F '\t' '{ print $3 }')"
  names="$(chain_prefix "$want_file")" || fail "stop file $want_file not in the chain"
  seg="$(jq -r '.stop_at.segment' <<<"$json")"
  pos="$(jq -r '.stop_at.pos' <<<"$json")"
  got="$(jq -r '[.segments[].name] | join(",")' <<<"$json")"
  [[ "$covered" == "true" && "$seg" == "$want_file" && "$pos" == "$want_pos" && "$got" == "$names" ]] || fail "$kind at=$at got seg=$seg pos=$pos names=$got want seg=$want_file pos=$want_pos names=$names"
}

echo "[coverage] time cases"
for at in "${INSIDE_AT[@]}"; do assert_time inside "$at"; done
for at in "${OUTSIDE_AT[@]}"; do assert_time outside "$at"; done
for at in "${BOUNDARY_AT[@]}"; do assert_time boundary "$at"; done

mapfile -t GTIDS < <(awk -F '\t' '$5 != "" { print $1 "\t" $2 "\t" $3 "\t" $5 }' "$WORKDIR/events.tsv")
[[ "${#GTIDS[@]}" -ge 20 ]] || fail "need 20 GTID events, got ${#GTIDS[@]}"

INSIDE_G=()
BOUNDARY_G=()
for base in "${DATA_FILES[@]}"; do
  mapfile -t rows < <(awk -F '\t' -v b="$base" '$5 != "" && $1 == b { print $5 }' "$WORKDIR/events.tsv")
  [[ "${#rows[@]}" -ge 4 ]] || fail "$base has ${#rows[@]} gtids"
  BOUNDARY_G+=("${rows[0]}" "${rows[${#rows[@]}-1]}")
  INSIDE_G+=("${rows[1]}" "${rows[2]}")
done
[[ "${#INSIDE_G[@]}" -eq 10 && "${#BOUNDARY_G[@]}" -eq 10 ]] || fail "gtid case count"
[[ "$(printf '%s\n' "${INSIDE_G[@]}" "${BOUNDARY_G[@]}" | sort -u | wc -l | tr -d ' ')" == "20" ]] || fail "gtid cases are not 20 distinct values"

uuid="${INSIDE_G[0]%%:*}"
min_seq=999999999
max_seq=0
for row in "${GTIDS[@]}"; do
  g="${row##*$'\t'}"
  seq="${g##*:}"
  if [[ "$seq" -lt "$min_seq" ]]; then min_seq="$seq"; fi
  if [[ "$seq" -gt "$max_seq" ]]; then max_seq="$seq"; fi
done
[[ "$min_seq" -gt 5 ]] || fail "expected earlier GTIDs before seq $min_seq"
OUTSIDE_G=()
for d in 1 2 3 4 5; do
  OUTSIDE_G+=("${uuid}:$((min_seq - d))")
  OUTSIDE_G+=("${uuid}:$((max_seq + d))")
done
# 5 before and 5 after, interleaved above, is 10.
[[ "${#OUTSIDE_G[@]}" -eq 10 ]] || fail "outside gtid count ${#OUTSIDE_G[@]}"

oracle_gtid() {
  local gtid="$1"
  awk -F '\t' -v g="$gtid" '
    $5 != "" {
      if ($5 == g) { file=$1; hit=1; nextpos="" }
      else if (hit && nextpos == "" && $1 == file) { nextpos=$2 }
    }
    END {
      if (!hit) { print "0\tgtid is not in the retained window"; exit }
      if (nextpos == "") {
        # end of this file: the last event end of this file
        print "1\t" file
      } else print "1\t" file "\t" nextpos
    }
  ' "$WORKDIR/events.tsv"
  # When nextpos is empty the caller reads the file size from the last event end.
}

file_end() {
  awk -F '\t' -v b="$1" '$1 == b { end=$3 } END { print end }' "$WORKDIR/events.tsv"
}

assert_gtid() {
  local kind="$1"
  local gtid="$2"
  local json covered reason want want_covered want_file want_pos seg pos got names
  json="$(curl -fsS -G "$API/api/tasks/$task_id/coverage" --data-urlencode "gtid=$gtid" --data-urlencode "limit=1")"
  covered="$(jq -r '.covered' <<<"$json")"
  reason="$(jq -r '.reason // empty' <<<"$json")"
  want="$(oracle_gtid "$gtid")"
  want_covered="${want%%$'\t'*}"
  if [[ "$want_covered" == "0" ]]; then
    [[ "$covered" == "false" && "$reason" == "gtid is not in the retained window" ]] || fail "$kind gtid=$gtid reason=$reason body=$json"
    return 0
  fi
  want_file="$(printf '%s' "$want" | awk -F '\t' '{ print $2 }')"
  want_pos="$(printf '%s' "$want" | awk -F '\t' '{ print $3 }')"
  if [[ -z "$want_pos" ]]; then
    want_pos="$(file_end "$want_file")"
  fi
  names="$(chain_prefix "$want_file")" || fail "gtid file $want_file not in the chain"
  seg="$(jq -r '.stop_at.segment' <<<"$json")"
  pos="$(jq -r '.stop_at.pos' <<<"$json")"
  got="$(jq -r '[.segments[].name] | join(",")' <<<"$json")"
  [[ "$covered" == "true" && "$seg" == "$want_file" && "$pos" == "$want_pos" && "$got" == "$names" ]] || fail "$kind gtid=$gtid got seg=$seg pos=$pos names=$got want seg=$want_file pos=$want_pos names=$names body=$json"
}

echo "[coverage] gtid cases"
for g in "${INSIDE_G[@]}"; do assert_gtid inside "$g"; done
for g in "${OUTSIDE_G[@]}"; do assert_gtid outside "$g"; done
for g in "${BOUNDARY_G[@]}"; do assert_gtid boundary "$g"; done

echo "[coverage] restore five points"
start_restore
i=0
for at in "${POINT_AT[@]}"; do
  plan="$(curl -fsS -X POST "$API/api/tasks/$task_id/restore-plan" -H 'Content-Type: application/json' -d "$(jq -n --arg at "$at" '{at:$at}')")"
  [[ "$(jq -r '.coverage.covered' <<<"$plan")" == "true" ]] || fail "plan not covered at $at: $plan"
  [[ "$(jq -r '.prechecks[] | select(.id=="window") | .result' <<<"$plan")" == "green" ]] || fail "window precheck: $plan"
  stop="$(printf '%s\n' "$(jq -r '.restore_sh' <<<"$plan")" | grep -oE -- "--stop-datetime='[^']+'" | head -1 || true)"
  [[ -n "$stop" ]] || fail "restore.sh has no stop-datetime: $(jq -r '.restore_sh' <<<"$plan")"
  want_stop="$(clock_shift "$at" "+ 1 second")"
  [[ "$stop" == "--stop-datetime='${want_stop}'" ]] || fail "stop $stop want $want_stop"
  reset_restore_schema
  args=()
  nfile=0
  while IFS= read -r p; do
    [[ -z "$p" || "$p" == "null" ]] && continue
    abs="$(abs_path "$p")"
    dest="/tmp/coverage-apply-${i}-${nfile}.bin"
    docker compose -f "$COMPOSE_FILE" cp "$abs" "percona80:${dest}" </dev/null
    args+=("$dest")
    nfile=$((nfile + 1))
  done < <(jq -r '.coverage.segments[].local_path' <<<"$plan")
  [[ "$nfile" -ge 1 ]] || fail "plan has no local paths"
  log="$(mktemp)"
  if ! docker compose -f "$COMPOSE_FILE" exec -T -e TZ=UTC percona80 \
      mysqlbinlog --verify-binlog-checksum --stop-datetime="$want_stop" "${args[@]}" \
      | docker exec -i "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp --binary-mode >"$log" 2>&1; then
    cat "$log" >&2
    rm -f "$log"
    fail "apply failed at $at"
  fi
  rm -f "$log"
  got="$(docker exec "$RESTORE" mysql -uroot -proot -h127.0.0.1 --protocol=tcp -Nse "CHECKSUM TABLE ${DB}.t" | awk '{ print $2 }' | tr -d '\r')"
  [[ "$got" == "${POINT_SUMS[$i]}" ]] || fail "checksum at $at got $got want ${POINT_SUMS[$i]}"
  echo "[coverage] restore $at checksum $got"
  i=$((i + 1))
done

echo "[coverage] full backup prechecks"
plan="$(curl -fsS -X POST "$API/api/tasks/$task_id/restore-plan" -H 'Content-Type: application/json' -d "$(jq -n --arg at "${POINT_AT[0]}" '{at:$at,full_backup_gtid:"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:1-9"}')")"
[[ "$(jq -r '.prechecks[] | select(.id=="full_backup_gtid") | .result' <<<"$plan")" == "red" ]] || fail "hole precheck: $plan"
pre="$(jq -r '.prerequisites[0]' <<<"$plan")"
applied="$(printf '%s\n' "$pre" | sed -n 's/.*must not intersect //p')"
[[ -n "$applied" && "$applied" != "(empty)" ]] || fail "applied set missing: $pre"
overlap="$(curl -fsS -X POST "$API/api/tasks/$task_id/restore-plan" -H 'Content-Type: application/json' -d "$(jq -n --arg at "${POINT_AT[4]}" --arg g "$applied" '{at:$at,full_backup_gtid:$g}')")"
desc="$(jq -r '.prechecks[] | select(.id=="full_backup_gtid") | .result + " " + .description' <<<"$overlap")"
[[ "$desc" == red*overlap* ]] || fail "overlap precheck: $desc"

echo "[coverage] passed task=$task_id"
