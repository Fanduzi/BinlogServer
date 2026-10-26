#!/usr/bin/env bash
# input: the e2e topology (run-suite.sh), python3, and E2E_OTLP_PORT (default 14318)
# output: smoke-observability run against a binlog-server with OTLP/HTTP tracing on (sample ratio 1) exporting to otlp-sink.py, so the scenario asserts both /metrics and exported API spans
# pos: entry point of make e2e-observability and the e2e-observability CI job (production-readiness F3)
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PORT="${E2E_OTLP_PORT:-14318}"
SINK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/binlog-e2e-otlp.XXXXXX")"

python3 "$ROOT_DIR/scripts/e2e/otlp-sink.py" "$PORT" "$SINK_DIR" &
SINK_PID=$!
trap 'kill "$SINK_PID" >/dev/null 2>&1 || true' EXIT

for _ in {1..50}; do
  if curl -s -o /dev/null -X POST "http://127.0.0.1:${PORT}/ready" 2>/dev/null; then
    break
  fi
  sleep 0.1
done

export BINLOG_SERVER_TRACING_ENABLED=true
export BINLOG_SERVER_TRACING_EXPORTER=otlp-http
export BINLOG_SERVER_TRACING_ENDPOINT="http://127.0.0.1:${PORT}/v1/traces"
export BINLOG_SERVER_TRACING_SAMPLE_RATIO=1
export BINLOG_SERVER_TRACING_SERVICE_NAME=binlog-server-e2e
# Flush batches quickly so the scenario sees spans while the server runs.
export OTEL_BSP_SCHEDULE_DELAY=500
export E2E_OTLP_SINK_DIR="$SINK_DIR"

"$ROOT_DIR/scripts/e2e/run-suite.sh" --scenarios smoke-observability
