#!/bin/bash
# PreToolUse Read|Edit 驗證
# 依據工具類型與路徑權限矩陣決定是否允許

HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HOOK_DIR/common.sh"

INPUT=$(cat)
TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty')
FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.file_path // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd')

if [ -z "$FILE_PATH" ]; then
  exit 0
fi

if [ -z "$CWD" ]; then
  deny_pretooluse "缺少工作目錄上下文"
fi
CWD=$(canonicalize_existing_dir "$CWD")
if [ -z "$CWD" ]; then
  deny_pretooluse "無法解析工作目錄"
fi

TARGET_PATH=$(canonicalize_path "$CWD" "$FILE_PATH")
if [ -z "$TARGET_PATH" ]; then
  deny_pretooluse "無法解析目標路徑"
fi

# plugin scope：禁止讀取 CLAUDE.md，允許唯讀存取 shared 目錄
if [ "$TASK_SCOPE" = "plugin" ]; then
  BASENAME=$(basename "$TARGET_PATH")
  if [ "$BASENAME" = "CLAUDE.md" ]; then
    deny_pretooluse "plugin scope 禁止讀取 CLAUDE.md"
  fi
  VR="${VAULT_ROOT:-/vaults}"
  SHARED_ROOT="$VR/shared"
  if path_within_root "$TARGET_PATH" "$SHARED_ROOT"; then
    ACTION=$(classify_tool_action "${TOOL_NAME:-Edit}")
    case "$ACTION" in
      write|delete|move)
        deny_pretooluse "${SHARED_ROOT}/ 是唯讀目錄，禁止編輯"
        ;;
    esac
    exit 0
  fi
fi

if ! path_within_root "$TARGET_PATH" "$CWD"; then
  deny_pretooluse "禁止存取工作目錄範圍外的路徑"
fi

# 依工具類型分類操作並檢查權限矩陣
ACTION=$(classify_tool_action "${TOOL_NAME:-Edit}")
check_and_enforce_permission "$TARGET_PATH" "$CWD" "$ACTION"

exit 0
