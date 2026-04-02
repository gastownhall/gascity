#!/bin/bash
# pack-update.sh — Sync knowledge, changelog, and docs to deepwork-org-config-pack
#
# Copies latest knowledge files, changelog, and docs from mayor/ to the
# org config pack repo, commits, and pushes to both Gitea and GitHub.
#
# Cron: 0 */6 * * * /home/pratham2/gt/mayor/scripts/pack-update.sh
# (runs after knowledge-evolve which is also every 6h)

set -uo pipefail

GT_ROOT="${GT_ROOT:-$HOME/gt}"
PACK_DIR="/tmp/deepwork-org-config-pack"
GITEA_REMOTE="http://gt-local:d43a23e8baa469cadb482fe8f13283f1c45f61a9@localhost:3300/Deepwork-AI/deepwork-org-config-pack.git"
GITHUB_REMOTE="https://github.com/masti-ai/deepwork-org-config-pack.git"
LOGFILE="$GT_ROOT/logs/pack-update.log"
LOCKFILE="/tmp/pack-update.lock"

mkdir -p "$(dirname "$LOGFILE")"
log() { echo "$(date +%Y-%m-%dT%H:%M:%S) $*" >> "$LOGFILE"; }

exec 200>"$LOCKFILE"
flock -n 200 || { log "SKIP — another update running"; exit 0; }

log "=== Starting pack update ==="

# Clone if not present
if [ ! -d "$PACK_DIR/.git" ]; then
  git clone "$GITEA_REMOTE" "$PACK_DIR" 2>/dev/null || { log "ERROR: clone failed"; exit 1; }
fi

cd "$PACK_DIR"
git fetch origin 2>/dev/null
git reset --hard origin/main 2>/dev/null

# --- Sync knowledge files ---
mkdir -p knowledge
for f in patterns.md anti-patterns.md decisions.md operations.md products.md; do
  src="$GT_ROOT/mayor/knowledge/$f"
  [ -f "$src" ] && cp "$src" "knowledge/$f"
done

# --- Sync changelog ---
mkdir -p docs/changelog
for f in "$GT_ROOT"/mayor/changelog/*.md; do
  [ -f "$f" ] && cp "$f" "docs/changelog/$(basename "$f")"
done

# --- Sync wasteland onboarding ---
mkdir -p docs/wasteland
[ -f "$GT_ROOT/docs/wasteland/ONBOARDING.md" ] && cp "$GT_ROOT/docs/wasteland/ONBOARDING.md" "docs/wasteland/ONBOARDING.md"

# --- Sync formulas ---
mkdir -p formulas
for f in mol-polecat-work mol-do-work mol-dog-wasteland-sync mol-polecat-base mol-scoped-work; do
  src="$GT_ROOT/.beads/formulas/${f}.formula.toml"
  [ -f "$src" ] && cp "$src" "formulas/${f}.formula.toml"
done

# --- Check for changes ---
git add -A
if git diff --cached --quiet; then
  log "No changes to push"
  exit 0
fi

# --- Commit and push ---
CHANGES=$(git diff --cached --stat | tail -1)
git commit -m "auto: pack update $(date +%Y-%m-%dT%H:%M) — $CHANGES" 2>/dev/null

git push origin main 2>/dev/null && log "OK: pushed to Gitea" || log "ERROR: Gitea push failed"

# Also push to GitHub
git remote set-url origin "$GITHUB_REMOTE" 2>/dev/null
git push origin main 2>/dev/null && log "OK: pushed to GitHub" || log "ERROR: GitHub push failed"
git remote set-url origin "$GITEA_REMOTE" 2>/dev/null

log "=== Pack update complete: $CHANGES ==="
