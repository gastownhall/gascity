#!/bin/bash
# knowledge/capture.sh — Append a knowledge entry to the appropriate file
#
# Usage: capture.sh <type> <title> <body> [source]
#   type:   pattern | anti-pattern | decision | operations | product
#   title:  Short title
#   body:   What we learned (can be multi-line)
#   source: Optional bead ID or session reference
#
# Called by: gt plugins, hooks, or agents directly
# Idempotent: skips if title already exists in target file

set -euo pipefail

TYPE="${1:?Usage: capture.sh <type> <title> <body> [source]}"
TITLE="${2:?Missing title}"
BODY="${3:?Missing body}"
SOURCE="${4:-unknown}"
DATE=$(date +%Y-%m-%d)

KB_DIR="$(dirname "$0")"

case "$TYPE" in
  pattern)       FILE="$KB_DIR/patterns.md" ;;
  anti-pattern)  FILE="$KB_DIR/anti-patterns.md" ;;
  decision)      FILE="$KB_DIR/decisions.md" ;;
  operations)    FILE="$KB_DIR/operations.md" ;;
  product)       FILE="$KB_DIR/products.md" ;;
  *)             echo "ERROR: Unknown type '$TYPE'. Use: pattern|anti-pattern|decision|operations|product" >&2; exit 1 ;;
esac

# Idempotency: skip if title already exists
if grep -qF "### $TITLE" "$FILE" 2>/dev/null; then
  echo "SKIP: '$TITLE' already exists in $(basename "$FILE")"
  exit 0
fi

# Append entry
cat >> "$FILE" << EOF

### $TITLE ($DATE)
$BODY
Source: $SOURCE.
EOF

echo "OK: Added '$TITLE' to $(basename "$FILE")"
