#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
FRONTEND_DIR="$ROOT_DIR/frontend"
UI_STATIC_DIR="$ROOT_DIR/internal/ui/static"

echo "[ui] build frontend (base=/ui/)"
(
  cd "$FRONTEND_DIR"
  npm run build
)

echo "[ui] sync dist -> internal/ui/static"
mkdir -p "$UI_STATIC_DIR"
cp -R "$FRONTEND_DIR/dist/." "$UI_STATIC_DIR/"

echo "[ui] done: backend /ui/ now serves latest frontend build"
