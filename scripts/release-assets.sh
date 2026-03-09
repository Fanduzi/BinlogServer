#!/usr/bin/env bash
# input: repo source tree, Go toolchain, Node toolchain, and target version/platform settings
# output: local release asset directories, tar.gz archives, and checksums.txt under dist/<version>
# pos: local release packaging entrypoint bridging embedded UI build and multi-platform binaries
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PROJECT_NAME="${PROJECT_NAME:-binlog-server}"
VERSION="${VERSION:-dev}"
DIST_ROOT="${DIST_ROOT:-$ROOT_DIR/dist/$VERSION}"
TARGETS="${RELEASE_TARGETS:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64}"
BUILD_UI="${BUILD_UI:-1}"
UI_STATIC_DIR="$ROOT_DIR/internal/ui/static"
ui_backup_dir=""
checksum_cmd=()

restore_ui_static() {
  if [[ -n "$ui_backup_dir" && -d "$ui_backup_dir" ]]; then
    rm -rf "$UI_STATIC_DIR"
    mkdir -p "$UI_STATIC_DIR"
    cp -R "$ui_backup_dir/." "$UI_STATIC_DIR/" 2>/dev/null || true
    rm -rf "$ui_backup_dir"
  fi
}

resolve_checksum_cmd() {
  if command -v sha256sum >/dev/null 2>&1; then
    checksum_cmd=(sha256sum)
    return
  fi

  if command -v shasum >/dev/null 2>&1; then
    checksum_cmd=(shasum -a 256)
    return
  fi

  echo "[release] error: need sha256sum or shasum to generate checksums.txt" >&2
  exit 1
}

if [[ "$BUILD_UI" == "1" ]]; then
  ui_backup_dir="$(mktemp -d)"
  mkdir -p "$UI_STATIC_DIR"
  cp -R "$UI_STATIC_DIR/." "$ui_backup_dir/" 2>/dev/null || true
  trap restore_ui_static EXIT

  if [[ ! -x "$ROOT_DIR/frontend/node_modules/.bin/vite" ]]; then
    echo "[release] frontend deps missing; run npm ci"
    (
      cd "$ROOT_DIR/frontend"
      npm ci
    )
  fi
  "$ROOT_DIR/scripts/build-ui.sh"
fi

rm -rf "$DIST_ROOT"
mkdir -p "$DIST_ROOT"
resolve_checksum_cmd

for target in $TARGETS; do
  os="${target%/*}"
  arch="${target#*/}"
  artifact_name="${PROJECT_NAME}_${VERSION}_${os}_${arch}"
  stage_dir="$DIST_ROOT/$artifact_name"
  archive_path="$DIST_ROOT/$artifact_name.tar.gz"

  mkdir -p "$stage_dir"
  echo "[release] build $artifact_name"
  (
    cd "$ROOT_DIR"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
      go build -trimpath -o "$stage_dir/$PROJECT_NAME" ./cmd/binlog-server
  )

  echo "[release] archive $artifact_name.tar.gz"
  tar -C "$DIST_ROOT" -czf "$archive_path" "$artifact_name"
done

(
  cd "$DIST_ROOT"
  "${checksum_cmd[@]}" ./*.tar.gz > checksums.txt
)

echo "[release] done: $DIST_ROOT"
