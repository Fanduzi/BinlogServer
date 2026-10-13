#!/usr/bin/env bash
# input: internal/ui/static/index.html (Console, /ui/) and internal/ui/classic/index.html (classic console, /ui-classic/), the JS bundle graphs they load (static and dynamic imports), and Console markers in console/src
# output: non-zero exit when a loaded bundle in either graph is not valid ESM, or when a console/src PITR, recoverable-window, source-switch, storage-alert, or damaged-replay marker is missing from the /ui/ graph
# pos: release gate for the JS that Chrome loads from /ui/ and /ui-classic/; CI and the tag workflow both run this before publish
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
HTML="$ROOT_DIR/internal/ui/static/index.html"

if ! command -v node >/dev/null 2>&1; then
  echo "[ui] node is required to syntax-check the embedded Console bundle" >&2
  exit 1
fi

# bundle_graph <static dir> <url prefix> <list file>
# Writes every JS file index.html loads, plus the chunks those import
# (static and dynamic), and ESM-checks each one.
bundle_graph() {
  local dir="$1" prefix="$2" out="$3"
  if [[ ! -f "$dir/index.html" ]]; then
    echo "[ui] missing $dir/index.html" >&2
    exit 1
  fi
  UI_DIR="$dir" UI_PREFIX="$prefix" node >"$out" <<'JS'
const fs = require("fs");
const path = require("path");

const staticDir = process.env.UI_DIR;
const prefix = process.env.UI_PREFIX;
const html = fs.readFileSync(path.join(staticDir, "index.html"), "utf8");
const esc = prefix.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const roots = [...html.matchAll(new RegExp(`(?:src|href)="(${esc}[^"]+\\.js)"`, "g"))].map((m) => m[1]);
if (roots.length === 0) {
  console.error(`[ui] index.html references no ${prefix}*.js bundles`);
  process.exit(1);
}

const seen = new Set();
const queue = roots.slice();
while (queue.length) {
  const urlPath = queue.shift();
  if (seen.has(urlPath)) continue;
  seen.add(urlPath);
  if (!urlPath.startsWith(prefix)) {
    console.error(`[ui] bundle graph escaped ${prefix}: ${urlPath}`);
    process.exit(1);
  }
  const rel = urlPath.slice(prefix.length);
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

  local rel
  while IFS= read -r rel; do
    [[ -n "$rel" ]] || continue
    # node --check on a .js path parses as CommonJS and accepts the broken import
    # that Chrome rejects. The bundles are <script type="module">, so check ESM.
    if ! node --check --input-type=module < "$dir/$rel"; then
      echo "[ui] embedded bundle is not valid ESM: $prefix$rel" >&2
      exit 1
    fi
    echo "[ui] esm ok: $prefix$rel"
  done < "$out"
}

list="$(mktemp)"
classic_list="$(mktemp)"
trap 'rm -f "$list" "$classic_list"' EXIT

bundle_graph "$ROOT_DIR/internal/ui/static" /ui/ "$list"
bundle_graph "$ROOT_DIR/internal/ui/classic" /ui-classic/ "$classic_list"

ROOT_DIR="$ROOT_DIR" UI_LIST="$list" node <<'JS'
const fs = require("fs");
const path = require("path");

const root = process.env.ROOT_DIR;
const staticDir = path.join(root, "internal/ui/static");
const rels = fs.readFileSync(process.env.UI_LIST, "utf8").split("\n").filter(Boolean);
const bundle = rels.map((rel) => fs.readFileSync(path.join(staticDir, rel), "utf8")).join("\n");

// Quoted test ids must match the attribute value, not a longer id that shares the prefix.
// stop_datetime / start_datetime / stop_gtid / start_gtid_set are the query keys in console/src/lib/api.ts.
// 停止时间, 停止 GTID, and 已执行 GTID are the zh-CN labels.
const markers = [
  { text: '"task-recovery-window"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: "这条备份链有缺口", source: "console/src/locales/zh-CN.json" },
  { text: '"task-pitr"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-pitr-stop"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-pitr-gtid"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-pitr-start"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-pitr-executed"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-pitr-build"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-pitr-copy"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-pitr-download"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: "stop_datetime", source: "console/src/lib/api.ts" },
  { text: "start_datetime", source: "console/src/lib/api.ts" },
  { text: "stop_gtid", source: "console/src/lib/api.ts" },
  { text: "start_gtid_set", source: "console/src/lib/api.ts" },
  { text: "停止时间", source: "console/src/locales/zh-CN.json" },
  { text: "停止 GTID", source: "console/src/locales/zh-CN.json" },
  { text: "已执行 GTID", source: "console/src/locales/zh-CN.json" },
  { text: '"task-source-chain"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-source-continued"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-source-stopped"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-source-next"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: "保留这份备份", source: "console/src/locales/zh-CN.json" },
  { text: "kept copying", source: "console/src/locales/en.json" },
  { text: '"task-storage-alert"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: '"task-replay-damaged"', source: "console/src/features/task-detail/task-detail-sheet.tsx" },
  { text: "损坏的分段", source: "console/src/locales/zh-CN.json" },
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
    console.error(`[ui] missing marker source ${marker.source}: ${err.message}`);
    failed = true;
    continue;
  }
  // A quoted test id is single-quoted in console/src (prettier) and double-quoted in the bundle.
  const singleQuoted = marker.text.replace(/^"(.*)"$/, "'$1'");
  if (!source.includes(marker.text) && !source.includes(singleQuoted)) {
    console.error(`[ui] console source lost marker ${marker.text} (${marker.source})`);
    failed = true;
    continue;
  }
  if (!present(bundle, marker.text)) {
    console.error(`[ui] embedded Console is missing PITR marker ${marker.text} from ${marker.source}; run make ui-build and commit internal/ui/static`);
    failed = true;
  }
}
if (failed) process.exit(1);
console.log(`[ui] console markers ok (${rels.length} bundles)`);
JS
