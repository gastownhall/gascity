#!/bin/bash
# gitea-to-github.sh — Mirror Gitea repos to GitHub (masti-ai org)
#
# Runs hourly via cron. For each repo:
# 1. Bare-clone from Gitea (or fetch if cached)
# 2. Push --mirror to GitHub (masti-ai org)
# 3. If significant changes (>5 commits since last sync), flag for release
#
# Cron: 0 * * * * /home/pratham2/gt/mayor/scripts/gitea-to-github.sh
#
# Dependencies: git, gh (authenticated), curl, jq

set -uo pipefail
# NOTE: no set -e — individual repo failures should not kill the whole sync

# Config
GITEA_URL="http://localhost:3300"
GITEA_ORG="Deepwork-AI"
GITEA_TOKEN="d43a23e8baa469cadb482fe8f13283f1c45f61a9"
GITHUB_ORG="masti-ai"
# NOTE: Gitea org is Deepwork-AI, GitHub org is masti-ai. All mirrors go to masti-ai.
MIRROR_DIR="/tmp/gitea-github-mirrors"
LOCKFILE="/tmp/gitea-github-sync.lock"
LOGFILE="/home/pratham2/gt/logs/gitea-github-sync.log"
CHANGELOG_SCRIPT="/home/pratham2/gt/mayor/changelog/append.sh"
RELEASE_THRESHOLD=5  # commits since last sync to trigger release

# Repos to mirror (public-facing only, skip internal/private)
# Format: gitea_name:github_name (if different) or just gitea_name
REPOS=(
  "ai-planogram"
  "alc-ai-villa"
  "OfficeWorld"
  "deepwork-site:website"
  "media-studio"
  "products"
  "gt-mesh"
  "deepwork-org-config-pack"
  "command-center"
  ".github"
)

# --- Setup ---
mkdir -p "$MIRROR_DIR" "$(dirname "$LOGFILE")"

log() { echo "$(date +%Y-%m-%dT%H:%M:%S) $*" >> "$LOGFILE"; }

# Flock to prevent concurrent runs
exec 200>"$LOCKFILE"
if ! flock -n 200; then
  log "SKIP — another sync running"
  exit 0
fi

log "=== Starting Gitea→GitHub sync ==="

TOTAL_NEW_COMMITS=0
REPOS_WITH_CHANGES=()

for entry in "${REPOS[@]}"; do
  # Parse gitea_name:github_name
  gitea_name="${entry%%:*}"
  github_name="${entry##*:}"

  bare_dir="$MIRROR_DIR/${gitea_name}.git"
  gitea_clone_url="${GITEA_URL}/${GITEA_ORG}/${gitea_name}.git"
  github_push_url="https://github.com/${GITHUB_ORG}/${github_name}.git"

  log "Syncing ${gitea_name} → ${GITHUB_ORG}/${github_name}"

  # Step 1: Ensure GitHub repo exists
  if ! gh repo view "${GITHUB_ORG}/${github_name}" >/dev/null 2>&1; then
    log "  Creating ${GITHUB_ORG}/${github_name} on GitHub"
    gh api "orgs/${GITHUB_ORG}/repos" -X POST \
      -f name="${github_name}" \
      -f visibility="public" \
      -f description="Mirror of ${GITEA_ORG}/${gitea_name}" \
      >/dev/null 2>&1 || {
        log "  ERROR: Failed to create repo on GitHub"
        continue
      }
    sleep 2  # GitHub needs a moment
  fi

  # Step 2: Bare clone or fetch
  if [ -d "$bare_dir" ]; then
    cd "$bare_dir"
    # Count commits before fetch to detect changes
    before_count=$(git rev-list --count --all 2>/dev/null || echo 0)
    git fetch --prune origin 2>/dev/null || {
      log "  ERROR: fetch failed, re-cloning"
      rm -rf "$bare_dir"
      git clone --bare "$gitea_clone_url" "$bare_dir" 2>/dev/null || { log "  ERROR: clone failed"; continue; }
      cd "$bare_dir"
      before_count=0
    }
  else
    git clone --bare "$gitea_clone_url" "$bare_dir" 2>/dev/null || { log "  ERROR: clone failed"; continue; }
    cd "$bare_dir"
    before_count=0
  fi

  after_count=$(git rev-list --count --all 2>/dev/null || echo 0)
  new_commits=$((after_count - before_count))

  # Step 3: Push to GitHub
  # Set push URL
  git remote set-url --push origin "$github_push_url" 2>/dev/null || \
    git remote add github "$github_push_url" 2>/dev/null || true

  # Push all branches + tags (not --mirror which deletes remote-only branches)
  if git push --force --all "$github_push_url" 2>/dev/null && \
     git push --tags "$github_push_url" 2>/dev/null; then
    log "  OK: pushed (${new_commits} new commits)"
  else
    log "  ERROR: push failed for ${github_name}"
    continue
  fi

  # Track changes
  if [ "$new_commits" -gt 0 ]; then
    TOTAL_NEW_COMMITS=$((TOTAL_NEW_COMMITS + new_commits))
    REPOS_WITH_CHANGES+=("${github_name}(+${new_commits})")
  fi

  # Step 4: If significant changes, create a proper semver release
  if [ "$new_commits" -ge "$RELEASE_THRESHOLD" ]; then
    log "  Significant changes (${new_commits} commits) — creating release"

    # Determine next semver tag
    # Get latest semver tag (vX.Y.Z), default to v0.0.0
    latest_ver=$(git tag --sort=-version:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1)
    if [ -z "$latest_ver" ]; then
      latest_ver="v0.0.0"
    fi

    # Parse major.minor.patch
    major=$(echo "$latest_ver" | sed 's/v//' | cut -d. -f1)
    minor=$(echo "$latest_ver" | sed 's/v//' | cut -d. -f2)
    patch=$(echo "$latest_ver" | sed 's/v//' | cut -d. -f3)

    # Determine bump type from commit messages
    # - "feat" or "feature" → minor bump
    # - "fix" or "bug" → patch bump
    # - "BREAKING" → major bump
    has_breaking=$(git log --oneline -"${new_commits}" 2>/dev/null | grep -ci "BREAKING\|breaking change" || true)
    has_feat=$(git log --oneline -"${new_commits}" 2>/dev/null | grep -ci "^[a-f0-9]* feat" || true)

    if [ "$has_breaking" -gt 0 ]; then
      major=$((major + 1)); minor=0; patch=0
    elif [ "$has_feat" -gt 0 ]; then
      minor=$((minor + 1)); patch=0
    else
      patch=$((patch + 1))
    fi
    new_tag="v${major}.${minor}.${patch}"

    # Check tag doesn't already exist
    if git tag -l "$new_tag" | grep -q .; then
      new_tag="v${major}.${minor}.$((patch + 1))"
    fi

    # Generate structured changelog — group by type, filter noise
    features=$(git log --oneline -"${new_commits}" 2>/dev/null | grep -iE "^[a-f0-9]+ feat" | grep -vi "bd: backup\|beads backup\|merge remote" | head -10)
    fixes=$(git log --oneline -"${new_commits}" 2>/dev/null | grep -iE "^[a-f0-9]+ fix" | grep -vi "bd: backup\|beads backup\|merge remote" | head -10)
    other=$(git log --oneline -"${new_commits}" 2>/dev/null | grep -viE "^[a-f0-9]+ (feat|fix)|bd: backup|beads backup|merge remote|Merge branch|Merge remote" | head -10)

    notes_file=$(mktemp)
    {
      echo "## What's Changed"
      echo ""
      if [ -n "$features" ]; then
        echo "### Features"
        echo "$features" | sed 's/^[a-f0-9]* /- /'
        echo ""
      fi
      if [ -n "$fixes" ]; then
        echo "### Bug Fixes"
        echo "$fixes" | sed 's/^[a-f0-9]* /- /'
        echo ""
      fi
      if [ -n "$other" ]; then
        echo "### Other"
        echo "$other" | sed 's/^[a-f0-9]* /- /' | head -5
        echo ""
      fi
      echo "---"
      echo "**${new_commits} commits** since ${latest_ver}"
      echo ""
      echo "*Released by [Gas Town](https://github.com/steveyegge/gastown) auto-sync*"
    } > "$notes_file"

    # Create tag locally and push it
    git tag "$new_tag" 2>/dev/null
    git push "$github_push_url" "$new_tag" 2>/dev/null

    gh release create "$new_tag" \
      --repo "${GITHUB_ORG}/${github_name}" \
      --title "${new_tag}" \
      --notes-file "$notes_file" \
      2>/dev/null && log "  Release ${new_tag} created" || log "  WARN: release creation failed"
    rm -f "$notes_file"
  fi

  cd /tmp
done

# Step 5: Log summary
if [ ${#REPOS_WITH_CHANGES[@]} -gt 0 ]; then
  changes_summary="${REPOS_WITH_CHANGES[*]}"
  log "Sync complete: ${TOTAL_NEW_COMMITS} new commits across ${#REPOS_WITH_CHANGES[@]} repos: ${changes_summary}"

  # Log to town changelog
  bash "$CHANGELOG_SCRIPT" "deploy" "town" \
    "GitHub mirror sync: ${TOTAL_NEW_COMMITS} commits" \
    "Repos updated: ${changes_summary}" 2>/dev/null || true
else
  log "Sync complete: no new changes"
fi

log "=== Done ==="
