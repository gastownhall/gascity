#!/bin/sh
set -eu

rig_root="${1:?rig root required}"
work_dir="${2:?work dir required}"
agent="${3:-agent}"
mode="${4:---sync}"

mkdir -p "$(dirname "$work_dir")"

if [ -d "$work_dir/.git" ] || git -C "$work_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if [ "$mode" = "--sync" ]; then
    git -C "$work_dir" fetch --all --prune >/dev/null 2>&1 || true
  fi
  exit 0
fi

branch="gc/${agent}"
git -C "$rig_root" worktree add -B "$branch" "$work_dir" HEAD
