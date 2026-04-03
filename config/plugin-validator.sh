#!/bin/bash

set -euo pipefail

PLUGIN_DIR="${1:-}"

if [ -z "$PLUGIN_DIR" ]; then
  echo "缺少插件目錄參數"
  exit 1
fi

if [ ! -d "$PLUGIN_DIR" ]; then
  echo "插件目錄不存在"
  exit 1
fi

append_error() {
  ERRORS="${ERRORS}$1\n"
}

ERRORS=""

entry_files=$(find "$PLUGIN_DIR" -maxdepth 1 -type f -name '*Plugin.tsx' | sort)
entry_count=$(printf '%s\n' "$entry_files" | sed '/^$/d' | wc -l | tr -d ' ')
all_entry_count=$(find "$PLUGIN_DIR" -type f -name '*Plugin.tsx' | wc -l | tr -d ' ')

if [ "$entry_count" -ne 1 ]; then
  append_error "入口檔必須且只能有 1 個，且必須位於插件目錄根層（*Plugin.tsx）"
fi

if [ "$all_entry_count" -ne "$entry_count" ]; then
  append_error "禁止在子目錄放置額外的 *Plugin.tsx"
fi

if find "$PLUGIN_DIR" -type f -name 'main.tsx' | grep -q .; then
  append_error "禁止使用 main.tsx，入口檔必須命名為 {Name}Plugin.tsx"
fi

file_count=$(find "$PLUGIN_DIR" -type f \( -name '*.tsx' -o -name '*.ts' -o -name '*.css' \) | wc -l | tr -d ' ')
if [ "$file_count" -lt 3 ]; then
  append_error "檔案數不足，至少需要 3 個 .tsx/.ts/.css 檔案"
fi

if find "$PLUGIN_DIR" -type f -name 'bundle.css' | grep -q .; then
  append_error "禁止使用 bundle.css，CSS 檔名必須有語意"
fi

if [ ! -f "$PLUGIN_DIR/bundle.js" ]; then
  append_error "bundle.js 缺失，必須先完成編譯"
else
  if grep -q '^import ' "$PLUGIN_DIR/bundle.js"; then
    append_error "bundle.js 含有 ES module import，必須使用 IIFE 編譯"
  fi
fi

if find "$PLUGIN_DIR" -type f \( -name '*.ts' -o -name '*.tsx' \) -print0 | xargs -0 grep -E -n "import[[:space:]]*\\{[^}]*\\bi18n\\b[^}]*\\}[[:space:]]*from[[:space:]]*['\"]@cubelv/sdk['\"]" >/dev/null 2>&1; then
  append_error "使用了被禁止的 i18n import，必須改成 i18next"
fi

if find "$PLUGIN_DIR" -type f \( -name '*.ts' -o -name '*.tsx' \) -print0 | xargs -0 grep -E -n "import[[:space:]]+React[[:space:]]+from[[:space:]]*['\"]react['\"]" >/dev/null 2>&1; then
  append_error "使用了被禁止的 React default import"
fi

if find "$PLUGIN_DIR" -type f \( -name '*.ts' -o -name '*.tsx' \) -print0 | xargs -0 grep -n "require(" >/dev/null 2>&1; then
  append_error "使用了被禁止的 require()"
fi

if [ -n "$ERRORS" ]; then
  printf '%b' "$ERRORS"
  exit 1
fi

entry_file=$(basename "$(printf '%s\n' "$entry_files" | sed '/^$/d' | head -n 1)")
printf 'ENTRY_FILE=%s\n' "$entry_file"
