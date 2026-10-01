#!/usr/bin/env bash
# input: internal/ui/static/index.html and the JS bundles it references
# output: non-zero exit when a referenced embedded Console bundle is not valid ESM
# pos: release gate for the JS that Chrome loads from /ui/
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
HTML="$ROOT_DIR/internal/ui/static/index.html"

if ! command -v node >/dev/null 2>&1; then
  echo "[ui] node is required to syntax-check the embedded Console bundle" >&2
  exit 1
fi

if [[ ! -f "$HTML" ]]; then
  echo "[ui] missing $HTML" >&2
  exit 1
fi

srcs_text="$(UI_HTML="$HTML" node <<'JS'
const fs = require("fs");
const html = fs.readFileSync(process.env.UI_HTML, "utf8");
const found = [...html.matchAll(/(?:src|href)="(\/ui\/[^"]+\.js)"/g)].map((m) => m[1]);
if (found.length === 0) {
  console.error("[ui] index.html references no /ui/*.js bundles");
  process.exit(1);
}
for (const src of found) console.log(src);
JS
)"
mapfile -t srcs <<< "$srcs_text"

for src in "${srcs[@]}"; do
  rel="${src#/ui/}"
  f="$ROOT_DIR/internal/ui/static/$rel"
  if [[ ! -f "$f" ]]; then
    echo "[ui] missing bundle referenced by index.html: $rel" >&2
    exit 1
  fi
  # node --check on a .js path parses as CommonJS and accepts the broken import
  # that Chrome rejects. The bundles are <script type="module">, so check ESM.
  if ! node --check --input-type=module < "$f"; then
    echo "[ui] embedded bundle is not valid ESM: $rel" >&2
    exit 1
  fi
  echo "[ui] esm ok: $rel"
done
