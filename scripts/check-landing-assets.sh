#!/usr/bin/env bash
# input: docs/landing/index.html and referenced assets in docs/landing and docs/images
# output: non-zero exit when landing page images are missing, zero-byte, or not valid PNG format
# pos: build and CI verification gate ensuring landing page console assets ship as real PNGs
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
LANDING_DIR="$ROOT_DIR/docs/landing"
HTML_FILE="$LANDING_DIR/index.html"

if [[ ! -f "$HTML_FILE" ]]; then
  echo "[landing-assets] error: landing page not found: $HTML_FILE" >&2
  exit 1
fi

if ! command -v node >/dev/null 2>&1; then
  echo "[landing-assets] error: node is required to inspect landing page asset references" >&2
  exit 1
fi

echo "[landing-assets] inspecting images referenced in $HTML_FILE"

# Extract all src and href attributes ending in an image extension
img_refs="$(node -e '
const fs = require("fs");
const html = fs.readFileSync(process.argv[1], "utf8");
const matches = [...html.matchAll(/(?:src|href)="([^"]+\.(?:png|jpe?g|gif|webp|svg))"/gi)].map(m => m[1]);
const unique = Array.from(new Set(matches));
if (unique.length === 0) {
  console.error("No image assets referenced in landing HTML");
  process.exit(1);
}
for (const ref of unique) {
  console.log(ref);
}
' "$HTML_FILE")"

mapfile -t refs <<< "$img_refs"

is_valid_png() {
  local f="$1"
  # PNG signature: 89 50 4E 47 0D 0A 1A 0A
  # Compare first 8 bytes
  local sig
  sig="$(head -c 8 "$f" | od -An -tx1 | tr -d ' \n')"
  [[ "$sig" == "89504e470d0a1a0a" ]]
}

failed=0

for ref in "${refs[@]}"; do
  # Ignore absolute external URLs if any
  if [[ "$ref" =~ ^https?:// ]]; then
    continue
  fi

  target_path="$LANDING_DIR/$ref"
  # Canonicalize path if possible
  canonical_target="$(realpath -m "$target_path")"

  if [[ ! -f "$canonical_target" ]]; then
    echo "[landing-assets] error: referenced asset does not exist: $ref -> $canonical_target" >&2
    failed=1
    continue
  fi

  if [[ ! -s "$canonical_target" ]]; then
    echo "[landing-assets] error: asset is zero bytes: $ref -> $canonical_target" >&2
    failed=1
    continue
  fi

  # Check PNG header if it is a .png
  if [[ "$ref" == *.png ]]; then
    if ! is_valid_png "$canonical_target"; then
      echo "[landing-assets] error: asset is not a valid PNG file (possibly text/html fallback): $ref -> $canonical_target" >&2
      failed=1
      continue
    fi
  fi

  echo "[landing-assets] ok: $ref (PNG valid, $(wc -c < "$canonical_target" | tr -d ' ') bytes)"
done

# Also verify that Pages deployment output directory contains both docs/landing/images and docs/landing/docs/images
echo "[landing-assets] verifying fallback directory compatibility for Cloudflare Pages"
for fallback_dir in "$LANDING_DIR/images" "$LANDING_DIR/docs/images"; do
  for img_name in console-dashboard.png task-detail.png swagger.png; do
    f="$fallback_dir/$img_name"
    if [[ ! -f "$f" ]]; then
      echo "[landing-assets] error: missing fallback image: $f" >&2
      failed=1
    elif ! is_valid_png "$f"; then
      echo "[landing-assets] error: fallback image is not a valid PNG: $f" >&2
      failed=1
    fi
  done
done

if [[ "$failed" -ne 0 ]]; then
  echo "[landing-assets] FAILED: landing page assets check failed" >&2
  exit 1
fi

echo "[landing-assets] OK: all landing page images exist and are valid PNG assets"
