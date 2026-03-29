#!/bin/bash
# PreToolUse Bash 安全檢查
# 阻擋危險的 shell 命令

HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HOOK_DIR/common.sh"

INPUT=$(cat)
COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd')

if [ -z "$COMMAND" ]; then
  exit 0
fi

if [ -z "$VAULT_ROOT" ] || [ -z "$CWD" ]; then
  deny_pretooluse "缺少 vault 執行上下文"
fi

CWD=$(canonicalize_existing_dir "$CWD")
if [ -z "$CWD" ]; then
  deny_pretooluse "無法解析工作目錄"
fi

VR="$VAULT_ROOT"
SHARED_ROOT="$VR/shared"
NORMALIZED_COMMAND="$COMMAND"
NORMALIZED_COMMAND="${NORMALIZED_COMMAND//\$\{VAULT_ROOT\}/$VAULT_ROOT}"
NORMALIZED_COMMAND="${NORMALIZED_COMMAND//\$VAULT_ROOT/$VAULT_ROOT}"
NORMALIZED_COMMAND="${NORMALIZED_COMMAND//\$\{PWD\}/$CWD}"
NORMALIZED_COMMAND="${NORMALIZED_COMMAND//\$PWD/$CWD}"

if echo "$NORMALIZED_COMMAND" | grep -qE '(\$\(|`|\$\{?[A-Za-z_][A-Za-z0-9_]*\}?)'; then
  deny_pretooluse "禁止在 Bash 中使用未解析的 shell 變數或命令替換，請改用明確絕對路徑"
fi

if echo "$NORMALIZED_COMMAND" | grep -qE '(^|[[:space:];|&])(bash|sh)[[:space:]]+-c([[:space:]]|$)|(^|[[:space:];|&])(eval|source)([[:space:]]|$)|(^|[[:space:];|&])\.[[:space:]]'; then
  deny_pretooluse "禁止在 Bash hook 中執行巢狀 shell 或動態載入腳本"
fi

if echo "$NORMALIZED_COMMAND" | grep -qE '(^|[[:space:];|&])cd([[:space:]]|$)'; then
  deny_pretooluse "vault Bash 請直接使用絕對路徑，不要使用 cd"
fi

if echo "$NORMALIZED_COMMAND" | grep -qE '(^|[[:space:];|&])ln[[:space:]]+-s([[:space:]]|$)'; then
  deny_pretooluse "禁止建立 symlink"
fi

if echo "$NORMALIZED_COMMAND" | grep -qE '(^|[[:space:]'"'"'";|&()<>])(\./|\.\./|[A-Za-z0-9._-]+/)'; then
  deny_pretooluse "vault Bash 請使用絕對路徑，禁止使用相對路徑或目錄跳轉"
fi

# 規則 1：阻擋破壞性命令
DANGEROUS_PATTERNS=(
  "rm -rf /"
  "rm -rf ~"
  "rm -rf \$HOME"
  "mkfs"
  "dd if="
  "> /dev/sd"
)

for pattern in "${DANGEROUS_PATTERNS[@]}"; do
  if echo "$COMMAND" | grep -qF "$pattern"; then
    deny_pretooluse "偵測到危險命令模式 \"$pattern\""
  fi
done

# 規則 2：分類 Bash 命令操作類型
IS_WRITE_COMMAND=false
BASH_ACTION="read"
if echo "$NORMALIZED_COMMAND" | grep -qE '(^|[[:space:];|&])(rm|rmdir)([[:space:]]|$)'; then
  IS_WRITE_COMMAND=true
  BASH_ACTION="delete"
elif echo "$NORMALIZED_COMMAND" | grep -qE '(^|[[:space:];|&])mv([[:space:]]|$)'; then
  IS_WRITE_COMMAND=true
  BASH_ACTION="move"
elif echo "$NORMALIZED_COMMAND" | grep -qE '(>|>>|(^|[[:space:];|&])(cp|mkdir|touch|install|tee|chmod|chown|ln|sed[[:space:]]+-i|perl[[:space:]]+-pi)([[:space:]]|$))'; then
  IS_WRITE_COMMAND=true
  BASH_ACTION="write"
elif echo "$NORMALIZED_COMMAND" | grep -qE '(^|[[:space:];|&])(python3?|node|ruby|php)[[:space:]]'; then
  IS_WRITE_COMMAND=true
  BASH_ACTION="write"
fi

# 規則 3：Bash 只能存取當前工作目錄或 shared（shared 僅允許讀取）
while IFS= read -r raw_path; do
  [ -z "$raw_path" ] && continue

  CANONICAL_PATH=$(canonicalize_path "$CWD" "$raw_path")
  if path_within_root "$CANONICAL_PATH" "$SHARED_ROOT"; then
    if [ "$IS_WRITE_COMMAND" = "true" ]; then
      deny_pretooluse "${SHARED_ROOT}/ 是唯讀目錄，禁止寫入"
    fi
    continue
  fi

  if ! path_within_root "$CANONICAL_PATH" "$CWD"; then
    deny_pretooluse "禁止存取工作目錄外的 vault 路徑"
  fi

  # 檢查路徑權限矩陣
  check_and_enforce_permission "$CANONICAL_PATH" "$CWD" "$BASH_ACTION"
done < <(echo "$NORMALIZED_COMMAND" | tr -d "\"'" | grep -oE '/[^[:space:];|&()<>]+' || true)

exit 0
