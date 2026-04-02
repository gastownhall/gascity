#!/bin/bash
# wasteland-sync.sh — Catch-up reconciliation between beads and wasteland
#
# Runs every 4 hours via cron. Handles anything the hooks missed:
# 1. Scan all rig beads for P0/P1 not on wasteland → post them
# 2. Scan closed beads that have open wasteland items → mark done
# 3. Push to DoltHub
#
# Cron: 0 */4 * * * /home/pratham2/gt/mayor/scripts/wasteland-sync.sh

set -uo pipefail

GT_ROOT="${GT_ROOT:-$HOME/gt}"
LOCKFILE="/tmp/wasteland-sync.lock"
LOGFILE="$GT_ROOT/logs/wasteland-sync.log"
DOLT_CMD="dolt --host 127.0.0.1 --port 3307 --user root --password '' --no-tls"

mkdir -p "$(dirname "$LOGFILE")"
log() { echo "$(date +%Y-%m-%dT%H:%M:%S) [sync] $*" >> "$LOGFILE"; }

# Flock
exec 200>"$LOCKFILE"
if ! flock -n 200; then
  log "SKIP — another sync running"
  exit 0
fi

log "=== Starting wasteland catch-up sync ==="

# Rig → project name mapping
declare -A RIG_PROJECT=(
  ["officeworld"]="officeworld"
  ["deepwork_site"]="deepwork-site"
  ["villa_alc_ai"]="alc-ai-villa"
  ["villa_ai_planogram"]="ai-planogram"
  ["command_center"]="command-center"
  ["products"]="products"
  ["media_studio"]="media-studio"
)

declare -A REPO_MAP=(
  ["officeworld"]="https://github.com/masti-ai/OfficeWorld"
  ["deepwork_site"]="https://github.com/masti-ai/website"
  ["villa_alc_ai"]="https://github.com/masti-ai/alc-ai-villa"
  ["villa_ai_planogram"]="https://github.com/masti-ai/ai-planogram"
  ["command_center"]="https://github.com/masti-ai/command-center"
  ["products"]="https://github.com/masti-ai/products"
  ["media_studio"]="https://github.com/masti-ai/media-studio"
)

# DB name → rig prefix mapping
declare -A DB_PREFIX=(
  ["officeworld"]="of"
  ["deepwork_site"]="ds"
  ["villa_alc_ai"]="vaa"
  ["villa_ai_planogram"]="vap"
  ["command_center"]="cc"
  ["products"]="prd"
  ["media_studio"]="med"
)

# Get existing wasteland items for dedup
wl_items=$(timeout 20 gt wl browse --json 2>/dev/null || echo "[]")
posted=0
closed=0

for rig in "${!RIG_PROJECT[@]}"; do
  db="$rig"
  prefix="${DB_PREFIX[$rig]}"
  project="${RIG_PROJECT[$rig]}"
  repo="${REPO_MAP[$rig]}"

  # Get open P0/P1 beads from this rig
  open_beads=$($DOLT_CMD sql -q "
    SELECT id, title, priority, issue_type, SUBSTRING(description, 1, 300) as desc_short
    FROM ${db}.issues
    WHERE status='open' AND priority <= 1
  " -r csv 2>/dev/null | tail -n +2) || continue

  while IFS=, read -r id title priority issue_type desc; do
    [ -z "$id" ] && continue
    bead_id="${prefix}-${id}"

    # Check if already on wasteland
    already=$(echo "$wl_items" | python3 -c "
import json,sys
items = json.load(sys.stdin)
for item in items:
    if 'Bead: ${bead_id}' in item.get('description','') or '${bead_id}' in item.get('title',''):
        print('yes')
        break
" 2>/dev/null)

    [ "$already" = "yes" ] && continue

    # Post it
    wl_type="feature"
    [[ "$issue_type" == "bug" ]] && wl_type="bug"

    timeout 15 gt wl post \
      --title "$title" \
      --project "$project" \
      --type "$wl_type" \
      --priority "$priority" \
      --description "Bead: ${bead_id}
Repo: ${repo}
Project: ${project}

${desc}" 2>/dev/null && { posted=$((posted+1)); log "Posted: $bead_id → wasteland"; } || log "ERROR: Failed to post $bead_id"

  done <<< "$open_beads"

  # Find closed beads that still have open wasteland items
  closed_beads=$($DOLT_CMD sql -q "
    SELECT id FROM ${db}.issues
    WHERE status='closed' AND priority <= 1
    AND closed_at > DATE_SUB(NOW(), INTERVAL 24 HOUR)
  " -r csv 2>/dev/null | tail -n +2) || continue

  while IFS= read -r id; do
    [ -z "$id" ] && continue
    bead_id="${prefix}-${id}"

    # Find open wasteland item for this bead
    wl_id=$(echo "$wl_items" | python3 -c "
import json,sys
items = json.load(sys.stdin)
for item in items:
    if 'Bead: ${bead_id}' in item.get('description','') and item.get('status','') == 'open':
        print(item['id'])
        break
" 2>/dev/null)

    [ -z "$wl_id" ] && continue

    timeout 10 gt wl claim "$wl_id" 2>/dev/null || true
    timeout 10 gt wl done "$wl_id" --evidence "Bead $bead_id closed locally" 2>/dev/null \
      && { closed=$((closed+1)); log "Closed: $wl_id (bead $bead_id)"; } || log "ERROR: Failed to close $wl_id"

  done <<< "$closed_beads"
done

# Push to DoltHub via SQL (server-compatible, no merge needed)
# gt wl sync does pull+merge which conflicts with running server
# Instead, push the wl-commons database directly via dolt push through SQL
dolt --host 127.0.0.1 --port 3307 --user root --password "" --no-tls sql -q "USE gt_collab; CALL dolt_push('origin', 'main')" 2>/dev/null \
  && log "DoltHub push OK" \
  || log "WARN: DoltHub push failed (may need manual gt wl sync with server stopped)"

log "Sync complete: $posted posted, $closed closed"
log "=== Done ==="
