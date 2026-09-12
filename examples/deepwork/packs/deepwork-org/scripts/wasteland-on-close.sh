#!/bin/bash
# wasteland-on-close.sh — Auto-mark wasteland items done when bead is closed
#
# Called after bd close. Finds the matching wasteland item and marks it done.
#
# Usage: wasteland-on-close.sh <bead-id>
# Environment: GT_ROOT (default ~/gt)

set -uo pipefail

BEAD_ID="${1:-}"
GT_ROOT="${GT_ROOT:-$HOME/gt}"
LOGFILE="$GT_ROOT/logs/wasteland-sync.log"
mkdir -p "$(dirname "$LOGFILE")"

log() { echo "$(date +%Y-%m-%dT%H:%M:%S) [on-close] $*" >> "$LOGFILE"; }

[ -z "$BEAD_ID" ] && exit 0

# Find matching wasteland item
wl_id=$(timeout 15 gt wl browse --json 2>/dev/null | python3 -c "
import json,sys
items = json.load(sys.stdin)
for item in items:
    desc = item.get('description','') + ' ' + item.get('title','')
    if 'Bead: $BEAD_ID' in desc or '$BEAD_ID' in item.get('title',''):
        if item.get('status','') == 'open':
            print(item['id'])
            break
" 2>/dev/null)

[ -z "$wl_id" ] && { log "SKIP: No wasteland item found for $BEAD_ID"; exit 0; }

# Claim it first (required before done)
timeout 10 gt wl claim "$wl_id" 2>/dev/null || true

# Mark done with evidence
timeout 10 gt wl done "$wl_id" --evidence "Bead $BEAD_ID closed locally" 2>/dev/null \
  && log "OK: Marked $wl_id done (bead $BEAD_ID)" \
  || log "ERROR: Failed to mark $wl_id done"

exit 0
