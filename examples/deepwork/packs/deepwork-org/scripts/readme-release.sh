#!/bin/bash
# readme-release.sh — Trigger LLM-powered release notes enrichment
#
# This script is a TRIGGER, not the enricher itself. It:
# 1. Checks if any repos have auto-generated releases from the hourly sync
# 2. If yes, dispatches the mol-dog-release-notes formula to the deacon
# 3. The dog (LLM agent) rewrites the notes with proper context
#
# The hourly gitea-to-github.sh creates releases with semver tags.
# This script ensures those releases get human-quality notes.
#
# Cron: 0 3 * * * (daily at 3 AM — after a day's worth of syncs)

set -uo pipefail

GT_ROOT="${GT_ROOT:-$HOME/gt}"
GITHUB_ORG="masti-ai"
LOGFILE="$GT_ROOT/logs/readme-release.log"
LOCKFILE="/tmp/readme-release.lock"

mkdir -p "$(dirname "$LOGFILE")"
log() { echo "$(date +%Y-%m-%dT%H:%M:%S) $*" >> "$LOGFILE"; }

exec 200>"$LOCKFILE"
flock -n 200 || { log "SKIP — another run"; exit 0; }

log "=== Checking for releases needing enrichment ==="

REPOS=(
  "ai-planogram"
  "alc-ai-villa"
  "OfficeWorld"
  "website"
  "media-studio"
  "products"
  "gt-mesh"
  "deepwork-org-config-pack"
  "command-center"
)

RELEASES_CREATED=0
needs_enrichment=0
for repo in "${REPOS[@]}"; do
  # Check latest release — does it have auto-generated notes?
  latest_notes=$(gh release view --repo "$GITHUB_ORG/$repo" --json body --jq '.body' 2>/dev/null || echo "")
  if echo "$latest_notes" | grep -qi "auto-sync\|auto-generated\|Gas Town.*auto"; then
    log "$repo: latest release has auto-generated notes"
    needs_enrichment=$((needs_enrichment + 1))
  fi
done

if [ "$needs_enrichment" -gt 0 ]; then
  log "$needs_enrichment repos need release note enrichment"

  # Dispatch the LLM dog formula (if deacon is alive)
  if tmux has-session -t hq-deacon 2>/dev/null; then
    timeout 15 bd create --rig gastown \
      "Release notes enrichment: $needs_enrichment repos need better notes" \
      -t chore --ephemeral \
      -l "type:dog-work,formula:mol-dog-release-notes" \
      -d "Repos with auto-generated release notes need LLM enrichment. Run mol-dog-release-notes formula." \
      --silent 2>/dev/null && log "Dispatched dog wisp for enrichment" || log "WARN: could not dispatch wisp"
  else
    log "WARN: deacon not running — enrichment will happen on next patrol"
  fi
else
  log "All releases have proper notes — nothing to do"
fi

log "=== Done ==="
