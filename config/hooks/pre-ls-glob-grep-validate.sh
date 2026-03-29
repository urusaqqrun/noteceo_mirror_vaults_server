#!/bin/bash
# PreToolUse LS|Glob|Grep|MultiEdit 路徑驗證
# 依據工具類型與路徑權限矩陣決定是否允許

HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HOOK_DIR/common.sh"

INPUT=$(cat)
TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd')

if [ -z "$CWD" ]; then
  deny_pretooluse "缺少工作目錄上下文"
fi
CWD=$(canonicalize_existing_dir "$CWD")
if [ -z "$CWD" ]; then
  deny_pretooluse "無法解析工作目錄"
fi

VR="${VAULT_ROOT:-/vaults}"
SHARED_ROOT="$VR/shared"

# 驗證路徑邊界 + 權限矩陣
check_path() {
  local raw_path="$1"
  local tool_action="$2"
  [ -z "$raw_path" ] && return 0

  local target
  target=$(canonicalize_path "$CWD" "$raw_path")
  [ -z "$target" ] && deny_pretooluse "無法解析目標路徑: $raw_path"

  # shared 目錄：僅允許讀取類操作
  if path_within_root "$target" "$SHARED_ROOT"; then
    case "$tool_action" in
      write|delete|move)
        deny_pretooluse "${SHARED_ROOT}/ 是唯讀目錄，禁止寫入"
        ;;
    esac
    return 0
  fi

  if ! path_within_root "$target" "$CWD"; then
    deny_pretooluse "禁止存取工作目錄範圍外的路徑: $raw_path"
  fi

  # 檢查路徑權限矩陣
  check_and_enforce_permission "$target" "$CWD" "$tool_action"
}

case "$TOOL_NAME" in
  LS)
    FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.path // empty')
    [ -z "$FILE_PATH" ] && exit 0
    check_path "$FILE_PATH" "list"
    ;;
  Glob)
    PATTERN=$(echo "$INPUT" | jq -r '.tool_input.pattern // empty')
    [ -z "$PATTERN" ] && exit 0
    PATH_PREFIX=$(echo "$PATTERN" | sed 's/[*?].*//')
    [ -n "$PATH_PREFIX" ] && check_path "$PATH_PREFIX" "list"
    ;;
  Grep)
    FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.path // empty')
    [ -z "$FILE_PATH" ] && exit 0
    check_path "$FILE_PATH" "search"
    ;;
  MultiEdit)
    FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.file_path // empty')
    [ -z "$FILE_PATH" ] && exit 0
    check_path "$FILE_PATH" "write"
    ;;
  *)
    exit 0
    ;;
esac

exit 0
