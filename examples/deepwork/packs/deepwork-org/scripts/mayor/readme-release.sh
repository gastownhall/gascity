#!/bin/bash
# readme-release.sh — Update README stats and create GitHub releases
#
# For each masti-ai repo on GitHub:
# 1. Check if there are new commits since last release
# 2. If 10+ commits, create a new release with changelog
#
# Also updates the org config pack README with current stats.
#
# Cron: 0 3 * * * /home/pratham2/gt/mayor/scripts/readme-release.sh
# (runs daily at 3 AM)

set -uo pipefail

GT_ROOT="${GT_ROOT:-$HOME/gt}"
GITHUB_ORG="masti-ai"
LOGFILE="$GT_ROOT/logs/readme-release.log"
LOCKFILE="/tmp/readme-release.lock"
CL_SCRIPT="$GT_ROOT/mayor/changelog/append.sh"
RELEASE_THRESHOLD=10

mkdir -p "$(dirname "$LOGFILE")"
log() { echo "$(date +%Y-%m-%dT%H:%M:%S) $*" >> "$LOGFILE"; }

exec 200>"$LOCKFILE"
flock -n 200 || { log "SKIP — another run"; exit 0; }

log "=== Starting README/release update ==="

REPOS=(
  "ai-planogram"
  "alc-ai-villa"
  "OfficeWorld"
  "website"
  "media-studio"
  "products"
  "gt-mesh"
  "deepwork-base"
  "command-center"
)

RELEASES_CREATED=0

for repo in "${REPOS[@]}"; do
  # Get latest release tag
  latest_tag=$(gh api "repos/$GITHUB_ORG/$repo/releases/latest" --jq '.tag_name' 2>/dev/null || echo "")

  # Count commits since last release (or all if no release)
  if [ -n "$latest_tag" ]; then
    commits_since=$(gh api "repos/$GITHUB_ORG/$repo/compare/${latest_tag}...main" --jq '.total_commits' 2>/dev/null || echo "0")
  else
    commits_since=$(gh api "repos/$GITHUB_ORG/$repo/commits?per_page=1" --jq 'length' 2>/dev/null || echo "0")
    # If no release exists and repo has commits, set high number to trigger
    [ "$commits_since" -gt 0 ] 2>/dev/null && commits_since=100
  fi

  log "$repo: $commits_since commits since ${latest_tag:-'no release'}"

  # Create release if threshold met
  if [ "${commits_since:-0}" -ge "$RELEASE_THRESHOLD" ] 2>/dev/null; then
    new_tag="v$(date +%Y.%m.%d)"

    # Check if tag already exists today
    if gh api "repos/$GITHUB_ORG/$repo/git/refs/tags/$new_tag" >/dev/null 2>&1; then
      log "  SKIP: $new_tag already exists"
      continue
    fi

    # Get recent commit messages
    changelog=$(gh api "repos/$GITHUB_ORG/$repo/commits?per_page=20" \
      --jq '.[].commit.message' 2>/dev/null | head -20 | sed 's/^/- /')

    notes_file=$(mktemp)
    cat > "$notes_file" <<EOF
## What's Changed

${changelog}

---
*${commits_since} commits since ${latest_tag:-'initial release'}*
EOF

    if gh release create "$new_tag" \
      --repo "$GITHUB_ORG/$repo" \
      --title "$repo $(date +%Y-%m-%d)" \
      --notes-file "$notes_file" 2>/dev/null; then
      log "  RELEASE: $new_tag created"
      RELEASES_CREATED=$((RELEASES_CREATED + 1))
    else
      log "  ERROR: release creation failed"
    fi
    rm -f "$notes_file"
  fi
done

# Log to changelog if releases were created
if [ "$RELEASES_CREATED" -gt 0 ]; then
  bash "$CL_SCRIPT" "deploy" "town" \
    "GitHub releases: $RELEASES_CREATED repos" \
    "Auto-created releases on masti-ai org" 2>/dev/null || true
fi

log "=== Done: $RELEASES_CREATED releases created ==="
