#!/bin/bash
# knowledge/on-bead-close.sh — Called after a bead is closed
#
# Usage: on-bead-close.sh <bead-id> <title> <close-reason>
#
# Decides whether the close is worth logging to changelog and/or knowledge base.
# Only logs if close_reason is substantial (>50 chars).

set -euo pipefail

BEAD_ID="${1:-}"
TITLE="${2:-}"
REASON="${3:-}"

[ -z "$BEAD_ID" ] && exit 0
[ ${#REASON} -lt 50 ] && exit 0

KB_DIR="$(dirname "$0")"
CL_DIR="$(dirname "$0")/../changelog"

# Always add to changelog
bash "$CL_DIR/append.sh" "fix" "town" "$TITLE" "Closed bead $BEAD_ID. $REASON" 2>/dev/null || true

# If it mentions a bug/incident/lesson, add to knowledge
if echo "$REASON" | grep -qiE 'bug|broke|incident|crash|fail|wrong|lesson|learned|avoid|never|always'; then
  bash "$KB_DIR/capture.sh" anti-pattern "$TITLE" "$REASON" "$BEAD_ID" 2>/dev/null || true
fi

exit 0
