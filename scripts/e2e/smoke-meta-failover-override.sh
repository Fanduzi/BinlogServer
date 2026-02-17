#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# 专项覆盖：显式使用非默认地址（localhost）验证 E2E_API/E2E_ORC_API 覆盖分支。
E2E_API_OVERRIDE="${E2E_API_OVERRIDE:-http://localhost:18080}"
E2E_ORC_API_OVERRIDE="${E2E_ORC_API_OVERRIDE:-http://localhost:13000/api}"

echo "[meta-failover-override] E2E_API=$E2E_API_OVERRIDE"
echo "[meta-failover-override] E2E_ORC_API=$E2E_ORC_API_OVERRIDE"

E2E_API="$E2E_API_OVERRIDE" \
E2E_ORC_API="$E2E_ORC_API_OVERRIDE" \
  "$ROOT_DIR/scripts/e2e/smoke-meta-failover.sh"
