#!/bin/bash

deny_pretooluse() {
  echo "Hook deny: $1" >&2
  jq -n --arg reason "$1" '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason: $reason
    }
  }'
  exit 2
}

canonicalize_path() {
  local base="$1"
  local target="$2"
  local raw_path

  if [[ "$target" == /* ]]; then
    raw_path="$target"
  else
    raw_path="$base/$target"
  fi

  local dir_path
  local base_name
  dir_path=$(dirname -- "$raw_path")
  base_name=$(basename -- "$raw_path")

  if [ -d "$raw_path" ]; then
    (cd "$raw_path" 2>/dev/null && pwd -P)
    return
  fi

  if [ -e "$raw_path" ] || [ -L "$raw_path" ]; then
    local canonical_dir
    canonical_dir=$(canonicalize_existing_dir "$dir_path") || return 1

    if [ -L "$raw_path" ]; then
      local link_target
      link_target=$(readlink "$raw_path") || return 1
      canonicalize_path "$canonical_dir" "$link_target"
      return
    fi

    printf '%s/%s\n' "$canonical_dir" "$base_name"
    return
  fi

  local canonical_dir
  canonical_dir=$(canonicalize_existing_dir "$dir_path") || return 1
  printf '%s/%s\n' "$canonical_dir" "$base_name"
}

canonicalize_existing_dir() {
  local path="$1"

  if [ -d "$path" ]; then
    (cd "$path" 2>/dev/null && pwd -P)
    return
  fi

  local parent_path
  local base_name
  parent_path=$(dirname -- "$path")
  base_name=$(basename -- "$path")

  local canonical_parent
  canonical_parent=$(canonicalize_existing_dir "$parent_path") || return 1

  if [ -L "$canonical_parent/$base_name" ]; then
    local link_target
    link_target=$(readlink "$canonical_parent/$base_name") || return 1
    canonicalize_path "$canonical_parent" "$link_target"
    return
  fi

  printf '%s/%s\n' "$canonical_parent" "$base_name"
}

path_within_root() {
  local path="$1"
  local root="$2"

  case "$path" in
    "$root"|"$root"/*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

# ── 路徑權限矩陣 ──

PERMISSIONS_FILE="/app/config/path-permissions.json"

# 將工具名稱轉換為操作類型
classify_tool_action() {
  local tool_name="$1"
  case "$tool_name" in
    Read)       echo "read" ;;
    LS)         echo "list" ;;
    Glob)       echo "list" ;;
    Grep)       echo "search" ;;
    Edit)       echo "write" ;;
    Write)      echo "write" ;;
    MultiEdit)  echo "write" ;;
    *)          echo "unknown" ;;
  esac
}

# 將絕對路徑轉為 vault 內相對路徑
get_vault_relative_path() {
  local target_path="$1"
  local cwd="$2"
  if [ "$target_path" = "$cwd" ]; then
    echo "."
    return
  fi
  echo "${target_path#$cwd/}"
}

# 依據權限矩陣檢查操作是否允許（return 0=允許, 1=拒絕）
enforce_path_permission() {
  local rel_path="$1"
  local action="$2"

  if [ ! -f "$PERMISSIONS_FILE" ]; then
    return 0
  fi

  local result
  result=$(jq -r --arg path "$rel_path" --arg action "$action" '
    . as $config |
    [($config.rules // [])[] | . as $r | select(
      ($path | startswith($r.prefix)) or ($path == ($r.prefix | rtrimstr("/")))
    )]
    | sort_by(.prefix | length) | reverse
    | (if length > 0 then .[0] else null end) as $rule
    | if $rule == null then
        ($config.default // {}) as $d |
        if (($d.deny // []) | any(. == $action)) then "denied"
        elif ($d.allow and (($d.allow // []) | any(. == $action) | not)) then "denied"
        else "allowed"
        end
      else
        if (($rule.deny // []) | any(. == $action)) then "denied"
        elif ($rule.allow and (($rule.allow // []) | any(. == $action) | not)) then "denied"
        else "allowed"
        end
      end
  ' "$PERMISSIONS_FILE" 2>/dev/null)

  [ "$result" != "denied" ]
}

# 統一權限驗證入口：路徑 + 操作，不通過則 deny
check_and_enforce_permission() {
  local target_path="$1"
  local cwd="$2"
  local action="$3"

  local rel_path
  rel_path=$(get_vault_relative_path "$target_path" "$cwd")

  if ! enforce_path_permission "$rel_path" "$action"; then
    deny_pretooluse "路徑 ${rel_path} 不允許執行 ${action} 操作"
  fi
}
