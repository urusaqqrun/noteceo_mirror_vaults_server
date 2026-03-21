#!/bin/bash
# PostToolUse Write|Edit 檢查
# 寫入後驗證檔案完整性，回饋給 Claude（無法 block，僅提醒修正）

HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HOOK_DIR/common.sh"

INPUT=$(cat)
FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.file_path // empty')

if [ -z "$FILE_PATH" ]; then
  exit 0
fi

# plugin scope 不檢查 vault 規則
if [ "$TASK_SCOPE" = "plugin" ]; then
  exit 0
fi

CWD=$(echo "$INPUT" | jq -r '.cwd')
if [ -z "$CWD" ]; then
  exit 0
fi
CWD=$(canonicalize_existing_dir "$CWD")
if [ -z "$CWD" ]; then
  exit 0
fi

FULL_PATH=$(canonicalize_path "$CWD" "$FILE_PATH")
if [ -z "$FULL_PATH" ]; then
  exit 0
fi

# 檢查 1：.md 檔案必須保留 frontmatter 中的 id 和 parentID
if [[ "$FILE_PATH" == *.md ]] && [ -f "$FULL_PATH" ]; then
  FIRST_LINE=$(head -1 "$FULL_PATH")
  if [ "$FIRST_LINE" = "---" ]; then
    HAS_ID=$(head -20 "$FULL_PATH" | grep -c "^id:")
    HAS_PARENT=$(head -20 "$FULL_PATH" | grep -c "^parentID:")
    if [ "$HAS_ID" -eq 0 ] || [ "$HAS_PARENT" -eq 0 ]; then
      MISSING=""
      [ "$HAS_ID" -eq 0 ] && MISSING="id"
      [ "$HAS_PARENT" -eq 0 ] && MISSING="${MISSING:+$MISSING、}parentID"
      jq -n --arg missing "$MISSING" '{
        decision: "block",
        reason: ("此 .md 檔案的 frontmatter 缺少 " + $missing + " 欄位。請確保 frontmatter 中保留 id 和 parentID。格式為 ---\\nid: xxx\\nparentID: xxx\\n---")
      }'
      exit 0
    fi
  fi
fi

# 檢查 2：_folder.json 必須保留 ID
if [[ "$FILE_PATH" == */_folder.json ]] && [ -f "$FULL_PATH" ]; then
  HAS_ID=$(jq -r '.ID // empty' "$FULL_PATH" 2>/dev/null)
  if [ -z "$HAS_ID" ]; then
    jq -n '{
      decision: "block",
      reason: "_folder.json 缺少 ID 欄位。請確保保留原始的 ID 欄位。"
    }'
    exit 0
  fi
fi

exit 0
