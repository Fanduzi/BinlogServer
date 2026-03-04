#!/usr/bin/env bash
# input: local go toolchain, make target e2e-quick, and repository source tree
# output: unified phase verification execution logs and per-step duration summary
# pos: repository-level hardening verification entry for test/race/vet/e2e quick gates
# note: if this file changes, update this header and module README.md.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$ROOT_DIR/tmp/phase-acceptance"
mkdir -p "$LOG_DIR"

declare -a SUMMARY=()

run_step() {
  local step_name="$1"
  local cmd="$2"
  local log_file="$3"
  local start_ts end_ts elapsed

  echo "==> [$step_name]"
  echo "cmd: $cmd"
  start_ts="$(date +%s)"
  if bash -lc "$cmd" 2>&1 | tee "$log_file"; then
    end_ts="$(date +%s)"
    elapsed="$((end_ts - start_ts))"
    SUMMARY+=("$step_name|PASS|${elapsed}s|$log_file")
    echo "--> [$step_name] PASS (${elapsed}s)"
  else
    end_ts="$(date +%s)"
    elapsed="$((end_ts - start_ts))"
    SUMMARY+=("$step_name|FAIL|${elapsed}s|$log_file")
    echo "--> [$step_name] FAIL (${elapsed}s)"
    exit 1
  fi
  echo
}

cd "$ROOT_DIR"

run_step "go test ./..." \
  "go test ./..." \
  "$LOG_DIR/go-test-all.log"

run_step "go test -race ./internal/tasks ./internal/api ./internal/replication" \
  "go test -race ./internal/tasks ./internal/api ./internal/replication" \
  "$LOG_DIR/go-test-race.log"

run_step "go vet ./..." \
  "go vet ./..." \
  "$LOG_DIR/go-vet.log"

run_step "make e2e-quick" \
  "make e2e-quick" \
  "$LOG_DIR/make-e2e-quick.log"

echo "==> Summary"
printf "%-70s %-8s %-8s %s\n" "Step" "Status" "Elapsed" "Log"
printf "%-70s %-8s %-8s %s\n" "----" "------" "-------" "---"
for row in "${SUMMARY[@]}"; do
  IFS="|" read -r name status elapsed log <<<"$row"
  printf "%-70s %-8s %-8s %s\n" "$name" "$status" "$elapsed" "$log"
done
