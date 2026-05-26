#!/usr/bin/env sh
set -eu

if ! command -v git >/dev/null 2>&1; then
  echo "git not found; stale branch sweep skipped"
  exit 0
fi

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "not inside a git work tree; stale branch sweep skipped"
  exit 0
fi

base_ref="${GC_STALE_BRANCH_BASE:-origin/main}"
if ! git rev-parse --verify "$base_ref" >/dev/null 2>&1; then
  echo "base ref $base_ref not found; stale branch sweep skipped"
  exit 0
fi

git for-each-ref --format='%(refname:short) %(committerdate:short)' refs/heads |
  while read -r branch date; do
    case "$branch" in
      main|master|trunk) continue ;;
    esac
    if git merge-base --is-ancestor "$branch" "$base_ref" >/dev/null 2>&1; then
      printf 'merged branch candidate: %s last_commit=%s base=%s\n' "$branch" "$date" "$base_ref"
    fi
  done
