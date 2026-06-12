#!/usr/bin/env bash
# Exec runtime-provider script for the openclaw-bridge demo (GC_SESSION=exec:<this>).
#
# Implements the gc exec provider contract (internal/runtime/exec/exec.go):
# first arg is the operation, second the session name; nudge content arrives
# on stdin. Unknown operations exit 2 (treated as success). Sessions are plain
# files under $DEMO_AGENT_STATE so the demo can show exactly what a session
# received.
set -euo pipefail

# No fallback: a silent default here would scatter session state outside the
# demo sandbox whenever env propagation breaks (e.g. a leaked supervisor).
STATE="${DEMO_AGENT_STATE:?DEMO_AGENT_STATE must be set (demo.sh exports it)}"
mkdir -p "$STATE"

op="${1:-}"
name="${2:-}"
safe="$(printf '%s' "$name" | tr -c 'A-Za-z0-9_.-' '_')"
base="$STATE/$safe"

case "$op" in
  start)
    cat >/dev/null || true
    : >"$base.running"
    touch "$base.nudges.log"
    ;;
  stop)
    rm -f "$base.running"
    ;;
  is-running)
    if [ -e "$base.running" ]; then echo true; else echo false; fi
    ;;
  process-alive)
    cat >/dev/null || true
    if [ -e "$base.running" ]; then echo true; else echo false; fi
    ;;
  watch-startup)
    # No startup dialogs to dismiss; close immediately.
    ;;
  nudge)
    {
      echo "----- nudge $(date -u +%Y-%m-%dT%H:%M:%SZ) -----"
      cat
      echo
    } >>"$base.nudges.log"
    ;;
  peek)
    lines="${3:-100}"
    if [ -e "$base.nudges.log" ]; then tail -n "$lines" "$base.nudges.log"; fi
    ;;
  list-running)
    prefix="${2:-}"
    for r in "$STATE"/*.running; do
      [ -e "$r" ] || continue
      b="$(basename "$r" .running)"
      case "$b" in "$prefix"*) echo "$b" ;; esac
    done
    ;;
  *)
    exit 2
    ;;
esac
