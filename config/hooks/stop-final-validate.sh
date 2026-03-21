#!/bin/bash
# Stop 最終驗證
# Claude 完成任務前執行全面檢查

INPUT=$(cat)
CWD=$(echo "$INPUT" | jq -r '.cwd')
STOP_ACTIVE=$(echo "$INPUT" | jq -r '.stop_hook_active')

if [ -n "$CWD" ]; then
  CWD=$(cd "$CWD" 2>/dev/null && pwd -P)
fi

append_error() {
  ERRORS="${ERRORS}$1\n"
}

# 防止無限循環：stop hook 已觸發過一次，直接放行
if [ "$STOP_ACTIVE" = "true" ]; then
  exit 0
fi

# plugin scope：檢查 main.tsx 入口是否存在
if [ "$TASK_SCOPE" = "plugin" ]; then
  ERRORS=""
  while IFS= read -r plugin_dir; do
    if [ ! -f "$plugin_dir/main.tsx" ]; then
      REL=$(echo "$plugin_dir" | sed "s|$CWD/||")
      append_error "${REL} 缺少 main.tsx 入口檔案"
    fi
  done < <(find "$CWD/plugins" -maxdepth 1 -mindepth 1 -type d 2>/dev/null)

  if [ -n "$ERRORS" ]; then
    REASON=$(echo -e "以下問題需要修正：\n$ERRORS")
    jq -n --arg reason "$REASON" '{
      decision: "block",
      reason: $reason
    }'
    exit 0
  fi
  exit 0
fi

# vault scope：全面檢查
ERRORS=""
CHECKED_PARENTS=""

# 檢查 1：Folder 不能同時包含 folder 和 note
while IFS= read -r folder_json; do
  DIR=$(dirname "$folder_json")
  PARENT=$(dirname "$DIR")

  # 跳過已檢查的目錄，避免重複報錯
  case "$CHECKED_PARENTS" in
    *"|$PARENT|"*) continue ;;
  esac
  CHECKED_PARENTS="${CHECKED_PARENTS}|$PARENT|"

  MD_COUNT=$(find "$PARENT" -maxdepth 1 -name "*.md" 2>/dev/null | wc -l)
  SUBFOLDER_COUNT=$(find "$PARENT" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | wc -l)

  if [ "$MD_COUNT" -gt 0 ] && [ "$SUBFOLDER_COUNT" -gt 0 ]; then
    REL=$(echo "$PARENT" | sed "s|$CWD/||")
    append_error "${REL}/ 同時包含筆記和子資料夾（不允許）"
  fi
done < <(find "$CWD" -name "_folder.json" -not -path "*/.NoteCEO/*" 2>/dev/null)

# 檢查 2：所有 .md 檔案必須有 frontmatter id 和 parentID
while IFS= read -r md_file; do
  FIRST_LINE=$(head -1 "$md_file")
  if [ "$FIRST_LINE" = "---" ]; then
    HAS_ID=$(head -20 "$md_file" | grep -c "^id:")
    HAS_PARENT=$(head -20 "$md_file" | grep -c "^parentID:")
    if [ "$HAS_ID" -eq 0 ] || [ "$HAS_PARENT" -eq 0 ]; then
      REL=$(echo "$md_file" | sed "s|$CWD/||")
      MISSING=""
      [ "$HAS_ID" -eq 0 ] && MISSING="id"
      [ "$HAS_PARENT" -eq 0 ] && MISSING="${MISSING:+$MISSING、}parentID"
      append_error "${REL} 缺少 frontmatter ${MISSING} 欄位"
    fi
  fi
done < <(find "$CWD" -name "*.md" -not -name "CLAUDE.md" -not -path "*/.NoteCEO/*" 2>/dev/null)

# 檢查 3：所有 _folder.json 必須保留 ID
while IFS= read -r folder_json; do
  HAS_ID=$(jq -r '.ID // empty' "$folder_json" 2>/dev/null)
  if [ -z "$HAS_ID" ]; then
    REL=$(echo "$folder_json" | sed "s|$CWD/||")
    append_error "${REL} 缺少 ID 欄位"
  fi
done < <(find "$CWD" -name "_folder.json" -not -path "*/.NoteCEO/*" 2>/dev/null)

# 檢查 4：禁止使用 symlink，避免繞過路徑隔離
while IFS= read -r symlink_path; do
  REL=$(echo "$symlink_path" | sed "s|$CWD/||")
  append_error "${REL} 是 symlink，不允許存在"
done < <(find "$CWD" -type l -not -path "*/.NoteCEO/*" 2>/dev/null)

if [ -n "$ERRORS" ]; then
  REASON=$(echo -e "以下問題需要修正：\n$ERRORS")
  jq -n --arg reason "$REASON" '{
    decision: "block",
    reason: $reason
  }'
  exit 0
fi

exit 0
