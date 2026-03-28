#!/bin/bash
# PreToolUse LS|Glob|Grep|MultiEdit 路徑驗證
# 禁止存取工作目錄（當前用戶 vault）範圍外的路徑

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

# 根據工具類型提取要檢查的路徑
check_path() {
  local raw_path="$1"
  [ -z "$raw_path" ] && return 0

  local target
  target=$(canonicalize_path "$CWD" "$raw_path")
  [ -z "$target" ] && deny_pretooluse "無法解析目標路徑: $raw_path"

  # shared 目錄允許唯讀存取
  if path_within_root "$target" "$SHARED_ROOT"; then
    return 0
  fi

  if ! path_within_root "$target" "$CWD"; then
    deny_pretooluse "禁止存取工作目錄範圍外的路徑: $raw_path"
  fi
}

case "$TOOL_NAME" in
  LS)
    FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.path // empty')
    if [ -z "$FILE_PATH" ]; then
      exit 0
    fi
    check_path "$FILE_PATH"
    ;;
  Glob)
    PATTERN=$(echo "$INPUT" | jq -r '.tool_input.pattern // empty')
    if [ -z "$PATTERN" ]; then
      exit 0
    fi
    # 從 glob pattern 提取路徑前綴（取 * 或 ? 之前的部分）
    PATH_PREFIX=$(echo "$PATTERN" | sed 's/[*?].*//')
    if [ -n "$PATH_PREFIX" ]; then
      check_path "$PATH_PREFIX"
    fi
    ;;
  Grep)
    FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.path // empty')
    if [ -z "$FILE_PATH" ]; then
      exit 0
    fi
    check_path "$FILE_PATH"
    ;;
  MultiEdit)
    FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.file_path // empty')
    if [ -z "$FILE_PATH" ]; then
      exit 0
    fi
    check_path "$FILE_PATH"
    ;;
  *)
    exit 0
    ;;
esac

exit 0
