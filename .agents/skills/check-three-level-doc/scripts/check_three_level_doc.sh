#!/usr/bin/env bash
set -euo pipefail

# Check three-level doc protocol on changed files.
# Default checks unstaged+staged diff against HEAD.
# Use --staged to check staged only.

MODE="all"
if [[ "${1:-}" == "--staged" ]]; then
  MODE="staged"
fi

if ! command -v git >/dev/null 2>&1; then
  echo "[three-level-doc] missing command: git" >&2
  exit 2
fi

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$ROOT" ]]; then
  echo "[three-level-doc] not in a git repository" >&2
  exit 2
fi
cd "$ROOT"

collect_changed_files() {
  if [[ "$MODE" == "staged" ]]; then
    git diff --cached --name-only --diff-filter=ACMR
  else
    {
      git diff --name-only --diff-filter=ACMR
      git diff --cached --name-only --diff-filter=ACMR
    } | sort -u
  fi
}

is_source_file() {
  local f="$1"
  case "$f" in
    *.go|*.sh|*.py|*.js|*.ts|*.tsx|*.java|*.rs|*.c|*.cc|*.cpp|*.h|*.hpp)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

has_l3_header() {
  local f="$1"
  local head
  head="$(sed -n '1,20p' "$f" 2>/dev/null || true)"
  printf '%s\n' "$head" | rg -q '^\s*(//|#)\s*input:' || return 1
  printf '%s\n' "$head" | rg -q '^\s*(//|#)\s*output:' || return 1
  printf '%s\n' "$head" | rg -q '^\s*(//|#)\s*pos:' || return 1
  printf '%s\n' "$head" | rg -q '^\s*(//|#)\s*note:' || return 1
  return 0
}

has_go_package_comment() {
  local f="$1"
  local head
  head="$(sed -n '1,30p' "$f" 2>/dev/null || true)"
  printf '%s\n' "$head" | rg -q '^\s*//\s*Package\s+[A-Za-z_][A-Za-z0-9_]*\b'
}

has_go_comment_order() {
  local f="$1"
  local head pkg_line input_line
  head="$(sed -n '1,30p' "$f" 2>/dev/null || true)"
  pkg_line="$(printf '%s\n' "$head" | nl -ba | rg -m1 '^\s*[0-9]+\s+//\s*Package\s+[A-Za-z_][A-Za-z0-9_]*\b' | awk '{print $1}')"
  input_line="$(printf '%s\n' "$head" | nl -ba | rg -m1 '^\s*[0-9]+\s*(//|#)\s*input:' | awk '{print $1}')"
  [[ -n "$pkg_line" && -n "$input_line" && "$pkg_line" -lt "$input_line" ]]
}

mapfile -t changed < <(collect_changed_files)
if [[ "${#changed[@]}" -eq 0 ]]; then
  echo "[three-level-doc] no changed files"
  exit 0
fi

# Build quick lookup map for changed files.
declare -A changed_map
for f in "${changed[@]}"; do
  changed_map["$f"]=1
done

status=0

# L3 checks.
for f in "${changed[@]}"; do
  [[ -f "$f" ]] || continue
  if ! is_source_file "$f"; then
    continue
  fi
  if ! has_l3_header "$f"; then
    echo "[three-level-doc][L3] missing/invalid header in: $f" >&2
    status=1
  fi
  if [[ "$f" == *.go ]] && ! has_go_package_comment "$f"; then
    echo "[three-level-doc][L3] missing/invalid Go package comment in: $f" >&2
    status=1
  fi
  if [[ "$f" == *.go ]] && ! has_go_comment_order "$f"; then
    echo "[three-level-doc][L3] invalid Go header order (Package must be before input): $f" >&2
    status=1
  fi
done

# L2 checks: if any source file in a module changed, module README.md must also be changed.
declare -A impacted_modules
for f in "${changed[@]}"; do
  [[ -f "$f" ]] || continue
  if ! is_source_file "$f"; then
    continue
  fi
  mod_dir="$(dirname "$f")"
  impacted_modules["$mod_dir"]=1
done

for mod in "${!impacted_modules[@]}"; do
  module_doc="$mod/README.md"
  if [[ ! -f "$module_doc" ]]; then
    echo "[three-level-doc][L2] missing module README.md: $module_doc" >&2
    status=1
    continue
  fi
  if [[ -z "${changed_map[$module_doc]:-}" ]]; then
    echo "[three-level-doc][L2] module changed but README.md not updated: $module_doc" >&2
    status=1
  fi
done

# L1 soft reminder: when module-level README changed, root README usually should be considered.
module_readme_changed=0
for f in "${changed[@]}"; do
  if [[ "$f" == */README.md && "$f" != "README.md" ]]; then
    module_readme_changed=1
    break
  fi
done
if [[ "$module_readme_changed" -eq 1 && -z "${changed_map[README.md]:-}" ]]; then
  echo "[three-level-doc][L1] reminder: module docs changed, confirm whether root README.md needs update" >&2
fi

if [[ "$status" -ne 0 ]]; then
  echo "[three-level-doc] FAILED" >&2
  exit "$status"
fi

echo "[three-level-doc] OK"
