#!/bin/bash
# wasteland-on-create.sh — Auto-post new beads to wasteland
#
# Called after bd create. Reads the new bead and posts to wasteland if P0/P1.
#
# Usage: wasteland-on-create.sh <bead-id> <rig>
# Environment: GT_ROOT (default ~/gt)

set -uo pipefail

BEAD_ID="${1:-}"
RIG="${2:-}"
GT_ROOT="${GT_ROOT:-$HOME/gt}"
LOGFILE="$GT_ROOT/logs/wasteland-sync.log"
mkdir -p "$(dirname "$LOGFILE")"

log() { echo "$(date +%Y-%m-%dT%H:%M:%S) [on-create] $*" >> "$LOGFILE"; }

[ -z "$BEAD_ID" ] && exit 0

# Get bead details
bead_json=$(timeout 10 bd show "$BEAD_ID" --json 2>/dev/null) || { log "SKIP: can't read $BEAD_ID"; exit 0; }
title=$(echo "$bead_json" | python3 -c "import json,sys; print(json.load(sys.stdin).get('title',''))" 2>/dev/null)
priority=$(echo "$bead_json" | python3 -c "import json,sys; print(json.load(sys.stdin).get('priority',2))" 2>/dev/null)
issue_type=$(echo "$bead_json" | python3 -c "import json,sys; print(json.load(sys.stdin).get('issue_type','task'))" 2>/dev/null)
description=$(echo "$bead_json" | python3 -c "import json,sys; print(json.load(sys.stdin).get('description','')[:500])" 2>/dev/null)

# Only sync P0/P1
[ "$priority" -gt 1 ] 2>/dev/null && { log "SKIP: $BEAD_ID is P${priority} (only P0/P1 synced)"; exit 0; }

# Map rig to project name and GitHub repo
declare -A REPO_MAP=(
  ["villa_ai_planogram"]="https://github.com/masti-ai/ai-planogram"
  ["villa_alc_ai"]="https://github.com/masti-ai/alc-ai-villa"
  ["officeworld"]="https://github.com/masti-ai/OfficeWorld"
  ["deepwork_site"]="https://github.com/masti-ai/website"
  ["products"]="https://github.com/masti-ai/products"
  ["media_studio"]="https://github.com/masti-ai/media-studio"
  ["command_center"]="https://github.com/masti-ai/command-center"
)

project="${RIG:-unknown}"
repo_url="${REPO_MAP[$RIG]:-}"

# Map issue_type to wasteland type
wl_type="feature"
[[ "$issue_type" == "bug" ]] && wl_type="bug"
[[ "$issue_type" == "docs" ]] && wl_type="docs"

# Check if already on wasteland (search by bead ID in description)
existing=$(timeout 10 gt wl browse --json 2>/dev/null | python3 -c "
import json,sys
items = json.load(sys.stdin)
for item in items:
    if 'Bead: $BEAD_ID' in (item.get('description','') + item.get('title','')):
        print(item['id'])
        break
" 2>/dev/null)

[ -n "$existing" ] && { log "SKIP: $BEAD_ID already on wasteland as $existing"; exit 0; }

# Estimate effort deterministically
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
effort=$(python3 "$SCRIPT_DIR/estimate-effort.py" "$title" 2>/dev/null || echo "medium")

# Post to wasteland with proper template
wl_desc="## Context
Repo: ${repo_url}
Project: ${project}
Bead: ${BEAD_ID}

## Task
${title}

${description}

## Acceptance Criteria
- Implementation matches the task description
- Tests pass
- No regressions
- PR submitted to main branch

## How to Work on This
1. Clone: git clone ${repo_url}
2. Branch: git checkout -b feat/your-change
3. Implement the change
4. Push + create PR
5. Submit: gt wl done <id> --evidence PR_URL"

timeout 15 gt wl post \
  --title "$title" \
  --project "$project" \
  --type "$wl_type" \
  --priority "$priority" \
  --effort "$effort" \
  --description "$wl_desc" 2>/dev/null && log "OK: Posted $BEAD_ID to wasteland (effort=$effort)" || log "ERROR: Failed to post $BEAD_ID"

exit 0
