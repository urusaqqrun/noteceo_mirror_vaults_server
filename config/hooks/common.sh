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
