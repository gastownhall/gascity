#!/bin/bash
# changelog/append.sh — Append an entry to the current month's changelog
#
# Usage: append.sh <type> <rigs> <title> <body>
#   type:  decision | deploy | fix | incident | milestone | infra
#   rigs:  comma-separated rig names, or "town"
#   title: Short title
#   body:  What happened (can be multi-line)
#
# Called by: agents, hooks, or plugins

set -euo pipefail

TYPE="${1:?Usage: append.sh <type> <rigs> <title> <body>}"
RIGS="${2:?Missing rigs}"
TITLE="${3:?Missing title}"
BODY="${4:?Missing body}"
DATE=$(date +%Y-%m-%d)
MONTH_FILE="$(dirname "$0")/$(date +%Y-%m).md"

# Create month file if it doesn't exist
if [ ! -f "$MONTH_FILE" ]; then
  echo "# $(date +'%B %Y')" > "$MONTH_FILE"
  echo "" >> "$MONTH_FILE"
fi

# Idempotency: skip if title already exists in this month
if grep -qF "— $TITLE" "$MONTH_FILE" 2>/dev/null; then
  echo "SKIP: '$TITLE' already in $(basename "$MONTH_FILE")"
  exit 0
fi

# Prepend entry after the header (newest first)
# Find line 2 (after the "# Month Year" header) and insert there
ENTRY="## $DATE — $TITLE

**Type:** $TYPE
**Rigs:** $RIGS

$BODY
"

# Insert after first line (the # header)
{
  head -1 "$MONTH_FILE"
  echo ""
  echo "$ENTRY"
  tail -n +2 "$MONTH_FILE"
} > "${MONTH_FILE}.tmp" && mv -f "${MONTH_FILE}.tmp" "$MONTH_FILE"

echo "OK: Added '$TITLE' to $(basename "$MONTH_FILE")"
