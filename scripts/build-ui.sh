#!/usr/bin/env bash
# input: frontend source tree, node toolchain, and build configuration dependencies
# output: compiled frontend assets synchronized into backend static serving directory; npm ci when vite is missing
# pos: build pipeline bridge between frontend artifacts and backend embedded UI delivery
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
FRONTEND_DIR="$ROOT_DIR/frontend"
UI_STATIC_DIR="$ROOT_DIR/internal/ui/static"

echo "[ui] build frontend (base=/ui/)"
(
  cd "$FRONTEND_DIR"
  if [[ ! -x node_modules/.bin/vite ]]; then
    echo "[ui] vite not found; running npm ci"
    npm ci
  fi
  npm run build
)

echo "[ui] sync dist -> internal/ui/static"
mkdir -p "$UI_STATIC_DIR"
cp -R "$FRONTEND_DIR/dist/." "$UI_STATIC_DIR/"

echo "[ui] done: backend /ui/ now serves latest frontend build"
