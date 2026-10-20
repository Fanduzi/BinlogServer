#!/usr/bin/env bash
# input: docker or a GitHub MinIO release binary, and the phase 3 verifier tests
# output: MinIO started on 127.0.0.1, then TestVerifyAgainstMinIO (delete one object, flip one object byte, flip one local byte)
# pos: real-object scenario for ADR 0009 phase 3; not in the quick profile and not selected by any CI job
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_TAG="$(date +%s)"
MINIO_NAME="binlog-e2e-minio-verify-${RUN_TAG}"
MINIO_PORT="${VERIFY_MINIO_PORT:-19010}"
MINIO_CONSOLE_PORT="${VERIFY_MINIO_CONSOLE_PORT:-19011}"
MINIO_USER="${VERIFY_MINIO_ACCESS_KEY:-minioadmin}"
MINIO_PASS="${VERIFY_MINIO_SECRET_KEY:-minioadmin}"
MINIO_BUCKET="${VERIFY_MINIO_BUCKET:-e2e-verify}"
MINIO_IMAGE="${MINIO_IMAGE:-quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z}"
MINIO_RELEASE="${MINIO_RELEASE:-RELEASE.2025-09-07T16-13-09Z}"
MINIO_PID=""
MINIO_BIN=""
MINIO_DATA_DIR=""

cleanup() {
  if [[ -n "$MINIO_PID" ]]; then
    kill "$MINIO_PID" >/dev/null 2>&1 || true
    wait "$MINIO_PID" >/dev/null 2>&1 || true
  fi
  docker rm -f -v "$MINIO_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_minio() {
  local _
  for _ in $(seq 1 60); do
    if curl -fsS "http://127.0.0.1:${MINIO_PORT}/minio/health/live" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "minio not ready" >&2
  return 1
}

linux_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *)
      echo "unsupported arch: $(uname -m)" >&2
      return 1
      ;;
  esac
}

start_minio() {
  docker rm -f -v "$MINIO_NAME" >/dev/null 2>&1 || true
  if docker pull "$MINIO_IMAGE" >/dev/null 2>&1; then
    docker run -d --name "$MINIO_NAME" \
      -p "${MINIO_PORT}:9000" \
      -e "MINIO_ROOT_USER=${MINIO_USER}" \
      -e "MINIO_ROOT_PASSWORD=${MINIO_PASS}" \
      "$MINIO_IMAGE" server /data --console-address ":9001" >/dev/null
    wait_minio
    return
  fi
  echo "[verify] quay image unavailable; using GitHub release ${MINIO_RELEASE}"
  local arch dir
  arch="$(linux_arch)"
  dir="/tmp/binlog-e2e-minio-bins"
  mkdir -p "$dir"
  MINIO_BIN="$dir/minio-${MINIO_RELEASE}"
  if [[ ! -x "$MINIO_BIN" ]]; then
    curl -fsSL -o "$MINIO_BIN" "https://github.com/minio/minio/releases/download/${MINIO_RELEASE}/minio.linux-${arch}.${MINIO_RELEASE}"
    chmod +x "$MINIO_BIN"
  fi
  MINIO_DATA_DIR="/tmp/binlog-e2e-minio-verify-${RUN_TAG}"
  mkdir -p "$MINIO_DATA_DIR"
  MINIO_ROOT_USER="$MINIO_USER" MINIO_ROOT_PASSWORD="$MINIO_PASS" \
    "$MINIO_BIN" server "$MINIO_DATA_DIR" --address "127.0.0.1:${MINIO_PORT}" --console-address "127.0.0.1:${MINIO_CONSOLE_PORT}" \
    >"/tmp/binlog-e2e-minio-verify-${RUN_TAG}.log" 2>&1 &
  MINIO_PID=$!
  wait_minio
}

start_minio
cd "$ROOT_DIR"
VERIFY_MINIO_ENDPOINT="127.0.0.1:${MINIO_PORT}" \
  VERIFY_MINIO_ACCESS_KEY="$MINIO_USER" \
  VERIFY_MINIO_SECRET_KEY="$MINIO_PASS" \
  VERIFY_MINIO_BUCKET="$MINIO_BUCKET" \
  go test -count=1 -run TestVerifyAgainstMinIO ./internal/assurance/
