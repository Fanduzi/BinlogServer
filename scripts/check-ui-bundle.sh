#!/usr/bin/env bash
# input: internal/ui/static/index.html, the JS bundle graph it loads, and PITR markers in frontend/src
# output: non-zero exit when a loaded Console bundle is not valid ESM, or when a frontend/src PITR marker is missing from that graph
# pos: release gate for the JS that Chrome loads from /ui/; CI and the tag workflow both run this before publish
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

list="$(mktemp)"
trap 'rm -f "$list"' EXIT

ROOT_DIR="$ROOT_DIR" node >"$list" <<'JS'
const fs = require("fs");
const path = require("path");

const root = process.env.ROOT_DIR;
const htmlPath = path.join(root, "internal/ui/static/index.html");
const staticDir = path.join(root, "internal/ui/static");
const html = fs.readFileSync(htmlPath, "utf8");
const roots = [...html.matchAll(/(?:src|href)="(\/ui\/[^"]+\.js)"/g)].map((m) => m[1]);
if (roots.length === 0) {
  console.error("[ui] index.html references no /ui/*.js bundles");
  process.exit(1);
}

const seen = new Set();
const queue = roots.slice();
while (queue.length) {
  const urlPath = queue.shift();
  if (seen.has(urlPath)) continue;
  seen.add(urlPath);
  if (!urlPath.startsWith("/ui/")) {
    console.error(`[ui] bundle graph escaped /ui/: ${urlPath}`);
    process.exit(1);
  }
  const rel = urlPath.slice("/ui/".length);
  const file = path.join(staticDir, rel);
  if (!fs.existsSync(file)) {
    console.error(`[ui] missing bundle referenced by index.html: ${rel}`);
    process.exit(1);
  }
  process.stdout.write(`${rel}\n`);
  const text = fs.readFileSync(file, "utf8");
  const dirUrl = urlPath.slice(0, urlPath.lastIndexOf("/"));
  const importRe = /(?:from|import\s*\()\s*["'](\.[^"']+\.js)["']/g;
  let match;
  while ((match = importRe.exec(text))) {
    queue.push(path.posix.normalize(path.posix.join(dirUrl, match[1])));
  }
}
JS

while IFS= read -r rel; do
  [[ -n "$rel" ]] || continue
  f="$ROOT_DIR/internal/ui/static/$rel"
  # node --check on a .js path parses as CommonJS and accepts the broken import
  # that Chrome rejects. The bundles are <script type="module">, so check ESM.
  if ! node --check --input-type=module < "$f"; then
    echo "[ui] embedded bundle is not valid ESM: $rel" >&2
    exit 1
  fi
  echo "[ui] esm ok: $rel"
done < "$list"

ROOT_DIR="$ROOT_DIR" UI_LIST="$list" node <<'JS'
const fs = require("fs");
const path = require("path");

const root = process.env.ROOT_DIR;
const staticDir = path.join(root, "internal/ui/static");
const rels = fs.readFileSync(process.env.UI_LIST, "utf8").split("\n").filter(Boolean);
const bundle = rels.map((rel) => fs.readFileSync(path.join(staticDir, rel), "utf8")).join("\n");

// Quoted test ids must match the attribute value, not a longer id that shares the prefix.
// stop_datetime / start_datetime / stop_gtid / start_gtid_set are the query keys in api.js.
// 停止时间, 停止 GTID, and 已执行 GTID are the zh-CN labels.
const markers = [
  { text: '"task-pitr"', source: "frontend/src/components/TaskDetailDrawer.vue" },
  { text: '"task-pitr-stop"', source: "frontend/src/components/TaskDetailDrawer.vue" },
  { text: '"task-pitr-gtid"', source: "frontend/src/components/TaskDetailDrawer.vue" },
  { text: '"task-pitr-start"', source: "frontend/src/components/TaskDetailDrawer.vue" },
  { text: '"task-pitr-executed"', source: "frontend/src/components/TaskDetailDrawer.vue" },
  { text: '"task-pitr-build"', source: "frontend/src/components/TaskDetailDrawer.vue" },
  { text: '"task-pitr-copy"', source: "frontend/src/components/TaskDetailDrawer.vue" },
  { text: '"task-pitr-download"', source: "frontend/src/components/TaskDetailDrawer.vue" },
  { text: "stop_datetime", source: "frontend/src/api.js" },
  { text: "start_datetime", source: "frontend/src/api.js" },
  { text: "stop_gtid", source: "frontend/src/api.js" },
  { text: "start_gtid_set", source: "frontend/src/api.js" },
  { text: "停止时间", source: "frontend/src/locales/zh-CN.json" },
  { text: "停止 GTID", source: "frontend/src/locales/zh-CN.json" },
  { text: "已执行 GTID", source: "frontend/src/locales/zh-CN.json" },
];

function present(haystack, text) {
  if (haystack.includes(text)) return true;
  let escaped = "";
  let changed = false;
  for (const ch of text) {
    const cp = ch.codePointAt(0);
    if (cp > 0x7e) {
      changed = true;
      escaped += "\\u" + cp.toString(16).padStart(4, "0");
    } else {
      escaped += ch;
    }
  }
  return changed && haystack.includes(escaped);
}

let failed = false;
for (const marker of markers) {
  const sourcePath = path.join(root, marker.source);
  let source = "";
  try {
    source = fs.readFileSync(sourcePath, "utf8");
  } catch (err) {
    console.error(`[ui] missing PITR source ${marker.source}: ${err.message}`);
    failed = true;
    continue;
  }
  if (!source.includes(marker.text)) {
    console.error(`[ui] frontend source lost PITR marker ${marker.text} (${marker.source})`);
    failed = true;
    continue;
  }
  if (!present(bundle, marker.text)) {
    console.error(`[ui] embedded Console is missing PITR marker ${marker.text} from ${marker.source}; run make ui-build and commit internal/ui/static`);
    failed = true;
  }
}
if (failed) process.exit(1);
console.log(`[ui] pitr markers ok (${rels.length} bundles)`);
JS
