#!/usr/bin/env bash
# input: mysql80 with GTID on, the suite API, the task data directory
# output: ADR 0009 §6 / design 0009 §628 GTID_HOLE acceptance (the #352
#   reproduction): a legal GTID_NEXT gap in a stored segment opens GTID_HOLE
#   on the task and turns /api/health 503; POST /api/alerts/waive with the
#   exact hole evidence closes it; the stored set growing afterwards keeps
#   the waiver in force; a new hole in a new segment reopens the finding
#   under new evidence
# pos: CI coverage for the evidence-bound GTID_HOLE waiver path end to end
#   (production-readiness A1)
# note: if this file changes, update this header and scripts/e2e/README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/e2e/docker-compose.yml"
API="${E2E_API:-http://127.0.0.1:18080}"
RUN_TAG="$(date +%s)"
TASK_ID=""

source "$ROOT_DIR/scripts/e2e/lib-migration.sh"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

need_cmd curl
need_cmd docker
need_cmd jq

mysql80() {
  docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -Nse "$1" | tr -d '\r'
}

cleanup() {
  if [[ -n "$TASK_ID" ]]; then
    curl -fsS -X POST "$API/api/tasks/$TASK_ID/stop" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

task_json() {
  curl -fsS "$API/api/tasks/$1"
}

wait_state() {
  local id="$1" want="$2" tries="${3:-120}" i st
  for i in $(seq 1 "$tries"); do
    st="$(task_json "$id" 2>/dev/null | jq -r '.state // empty')"
    if [[ "$st" == "$want" ]]; then
      return 0
    fi
    if [[ "$st" == "FAILED" ]]; then
      echo "task $id failed: $(task_json "$id")" >&2
      return 1
    fi
    sleep 1
  done
  echo "task $id never reached $want: $(task_json "$id")" >&2
  return 1
}

# holes prints every GTID_HOLE row (open and resolved) for the task, one
# compact JSON object per line. The evaluator sweep is the default 60s, so
# callers wait generously.
holes() {
  curl -fsS "$API/api/tasks/$1/findings?all=1" | jq -c '.[] | select(.kind == "GTID_HOLE")'
}

open_holes() {
  holes "$1" | jq -sc '[.[] | select(.resolved_at == null)]'
}

wait_hole_open() {
  local id="$1" secs="${2:-150}" i row
  for i in $(seq 1 "$secs"); do
    row="$(open_holes "$id" | jq -c '.[0] // empty')"
    if [[ -n "$row" ]]; then
      printf '%s' "$row"
      return 0
    fi
    sleep 1
  done
  echo "GTID_HOLE never opened on task $id" >&2
  curl -fsS "$API/api/tasks/$id/findings?all=1" >&2 || true
  return 1
}

wait_no_open_hole() {
  local id="$1" secs="${2:-150}" i n
  for i in $(seq 1 "$secs"); do
    n="$(open_holes "$id" | jq 'length')"
    if [[ "$n" == "0" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "GTID_HOLE still open on task $id" >&2
  curl -fsS "$API/api/tasks/$id/findings?all=1" >&2 || true
  return 1
}

# wait_new_open_segment waits until the task's live catalog row (state OPEN)
# is not the segment named by $2 — proof the source FLUSH reached the task
# and a new hole lands in a new segment. I3 reports only the first missing
# GNO per segment, so a hole written back into the old segment would stay
# masked by the waived one.
wait_new_open_segment() {
  local id="$1" old="$2" i name
  for i in $(seq 1 90); do
    name="$(curl -fsS "$API/api/tasks/$id/files" \
      | jq -r '[.[] | select(.state == "OPEN")][0].file_name // empty')"
    if [[ -n "$name" && "$name" != "$old" ]]; then
      printf '%s' "$name"
      return 0
    fi
    sleep 1
  done
  echo "task $id never opened a new segment after the source rotate" >&2
  curl -fsS "$API/api/tasks/$id/files" >&2 || true
  return 1
}

# gap_commit moves GTID_NEXT past one unused interval: it commits
# uuid:max+3 so uuid:max+1 and uuid:max+2 never execute — a hole in the
# stored set, the #352 shape. Prints the first missing GNO uuid:max+1
# (the evidence form an in-segment I3 reports) for the log.
gap_commit() {
  local uuid executed max k
  uuid="$(mysql80 "SELECT @@server_uuid;")"
  # The batch mysql client escapes embedded newlines as a literal \n; ask
  # the server to strip them so the per-UUID parse below sees real commas.
  executed="$(mysql80 "SELECT REPLACE(@@GLOBAL.GTID_EXECUTED, CHAR(10), '');" | tr -d ' \n')"
  max="$(printf '%s' "$executed" | tr ',' '\n' | grep -i "^${uuid}:" | sed -E 's/.*[-:]([0-9]+)$/\1/')"
  if [[ -z "$max" ]]; then
    echo "cannot read the last GNO of $uuid from $executed" >&2
    return 1
  fi
  k=$((max + 3))
  # AUTO_INCREMENT id: an explicit value would collide with the first gap's
  # row and the failing INSERT would burn GTID_NEXT without making a hole.
  mysql80 "SET GTID_NEXT='${uuid}:${k}'; INSERT INTO ${DB}.t(v) VALUES ('hole-${RUN_TAG}-${k}'); SET GTID_NEXT='AUTOMATIC';"
  printf '%s:%s' "$uuid" "$((max + 1))"
}

# hole_evidence reads the canonical evidence out of a finding's detail: an
# in-segment hole says "gtid hole in <seg> at uuid:n", an interior hole says
# "gtid hole: uuid:lo-hi is stored in no segment" — both carry the exact
# interval the waiver binds.
hole_evidence() {
  printf '%s' "$1" | grep -oiE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:[0-9]+(-[0-9]+)?' | head -1
}

waive_hole() {
  local subject="$1" evidence="$2" http
  http="$(curl -sS -o "/tmp/e2e-gtid-hole-waive-${RUN_TAG}.resp" -w '%{http_code}' -X POST "$API/api/alerts/waive" \
    -H 'Content-Type: application/json' \
    -d "{\"task_id\":\"$TASK_ID\",\"kind\":\"GTID_HOLE\",\"subject\":\"$subject\",\"evidence\":\"$evidence\",\"reason\":\"e2e legal skip\",\"actor\":\"e2e\"}")"
  if [[ "$http" != "200" ]]; then
    echo "waive rejected: http=$http body=$(cat /tmp/e2e-gtid-hole-waive-${RUN_TAG}.resp)" >&2
    return 1
  fi
}

DB="gtidhole_${RUN_TAG}"
mysql80 "CREATE DATABASE IF NOT EXISTS ${DB}; CREATE TABLE IF NOT EXISTS ${DB}.t(id INT PRIMARY KEY AUTO_INCREMENT, v VARCHAR(64));"

TASK_ID="$(curl -fsS -X POST "$API/api/tasks" -H 'Content-Type: application/json' \
  -d "{\"name\":\"e2e-gtid-hole-${RUN_TAG}\",\"cluster_key\":\"e2e-gtid-hole-${RUN_TAG}\",\"source\":{\"host\":\"$E2E_SOURCE_HOST\",\"port\":$E2E_MYSQL80_PORT,\"user\":\"$E2E_SOURCE_USER\",\"password\":\"$E2E_SOURCE_PASS\",\"flavor\":\"mysql\",\"server_id\":$((371000 + RANDOM % 9000))},\"start\":{\"mode\":\"LATEST\"},\"storage\":{\"retention_days\":7}}" \
  | jq -r '.id // empty')"
[[ -n "$TASK_ID" && "$TASK_ID" != "null" ]] || { echo "create task failed" >&2; exit 1; }
curl -fsS -X POST "$API/api/tasks/$TASK_ID/start" >/dev/null
wait_state "$TASK_ID" RUNNING
echo "[gtid-hole] task $TASK_ID RUNNING"

mysql80 "INSERT INTO ${DB}.t(v) VALUES ('seed-${RUN_TAG}');"
sleep 2

BASELINE_HEALTH="$(curl -sS -o /dev/null -w '%{http_code}' "$API/api/health")"
EVIDENCE1="$(gap_commit)"
mysql80 "INSERT INTO ${DB}.t(v) VALUES ('fill-${RUN_TAG}');"
echo "[gtid-hole] injected hole $EVIDENCE1 (baseline /api/health=$BASELINE_HEALTH)"

ROW="$(wait_hole_open "$TASK_ID")"
SUBJECT1="$(printf '%s' "$ROW" | jq -r '.subject')"
DETAIL="$(printf '%s' "$ROW" | jq -r '.detail')"
[[ -n "$SUBJECT1" && "$SUBJECT1" != "null" ]] || { echo "GTID_HOLE open without a subject: $ROW" >&2; exit 1; }
echo "[gtid-hole] GTID_HOLE open subject=$SUBJECT1 detail=$DETAIL"
EVIDENCE1="$(hole_evidence "$DETAIL")"
[[ -n "$EVIDENCE1" ]] || { echo "cannot read the hole evidence from detail: $DETAIL" >&2; exit 1; }
if [[ "$BASELINE_HEALTH" == "200" ]]; then
  [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$API/api/health")" == "503" ]] \
    || { echo "/api/health not 503 while GTID_HOLE open" >&2; exit 1; }
fi

waive_hole "$SUBJECT1" "$EVIDENCE1"
wait_no_open_hole "$TASK_ID"
echo "[gtid-hole] waived $EVIDENCE1 on $SUBJECT1: finding resolved"

# The stored set keeps growing past the hole; the waiver stays in force
# across sweeps.
mysql80 "INSERT INTO ${DB}.t(v) VALUES ('grow1-${RUN_TAG}'), ('grow2-${RUN_TAG}');"
for i in $(seq 1 15); do
  [[ "$(open_holes "$TASK_ID" | jq 'length')" == "0" ]] \
    || { echo "GTID_HOLE reopened while the stored set only grew" >&2; exit 1; }
  sleep 5
done
echo "[gtid-hole] waiver still in force after the stored set grew"

# A hole in a NEW segment is a different fact: new subject, new evidence,
# the finding reopens while the old waiver keeps its own hole closed. Wait
# for the task to consume the source rotate — a hole written before the
# local segment turns would land back in SUBJECT1 and stay masked by the
# waived first-missing GNO.
docker compose -f "$COMPOSE_FILE" exec -T mysql80 mysql -uroot -proot -e "FLUSH BINARY LOGS;" >/dev/null
wait_new_open_segment "$TASK_ID" "$SUBJECT1" >/dev/null
mysql80 "INSERT INTO ${DB}.t(v) VALUES ('seed2-${RUN_TAG}');"
sleep 2
EVIDENCE2="$(gap_commit)"
mysql80 "INSERT INTO ${DB}.t(v) VALUES ('fill2-${RUN_TAG}');"
echo "[gtid-hole] injected second hole $EVIDENCE2 after a source rotate"

ROW="$(wait_hole_open "$TASK_ID")"
SUBJECT2="$(printf '%s' "$ROW" | jq -r '.subject')"
DETAIL="$(printf '%s' "$ROW" | jq -r '.detail')"
[[ "$SUBJECT2" != "$SUBJECT1" ]] \
  || { echo "reopened finding kept the old subject $SUBJECT1 (want a new segment): $ROW" >&2; exit 1; }
EVIDENCE2="$(hole_evidence "$DETAIL")"
[[ -n "$EVIDENCE2" ]] || { echo "cannot read the hole evidence from detail: $DETAIL" >&2; exit 1; }
echo "[gtid-hole] reopened on the new hole subject=$SUBJECT2 evidence=$EVIDENCE2"
waive_hole "$SUBJECT2" "$EVIDENCE2"
wait_no_open_hole "$TASK_ID"
echo "[gtid-hole] success"
