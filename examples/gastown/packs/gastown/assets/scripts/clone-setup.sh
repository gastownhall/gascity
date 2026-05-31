#!/bin/sh
# clone-setup.sh — idempotent git clone for Gas City crew workspaces.
#
# Usage: clone-setup.sh <rig-root> <target-dir> <agent-name>
#
# Crew home is a full git clone of the rig repo (not a worktree). The clone
# uses --reference against the rig repo so it shares object storage and stays
# cheap on disk. Default branch matches the rig's origin/HEAD; the crew member
# is responsible for cutting their own feature branches per the Branch + PR
# workflow documented in crew.template.md.
#
# Called from pre_start in pack configs. Runs before the session is created
# so the agent starts IN the clone directory.

set -eu

RIG_ROOT="${1:?usage: clone-setup.sh <rig-root> <target-dir> <agent-name>}"
TARGET="${2:?missing target-dir}"
AGENT="${3:?missing agent-name}"

# The supervisor sets cwd to the work_dir (which is $TARGET) before running
# pre_start. The script rmdir's $TARGET before cloning into it; if we stayed
# inside it, git clone would fail with "Unable to read current working
# directory" once the cwd inode was unlinked. Step out to a stable parent.
cd /

# Idempotent: a present clone is left as-is. Crew sessions reuse the same
# workspace across nudges; clobbering would discard WIP and feature branches.
if [ -d "$TARGET/.git" ] || [ -f "$TARGET/.git" ]; then
    exit 0
fi

# Resolve the rig's origin URL — required for a real clone with a working
# remote. If the rig has no origin, fall back to a local-path clone (still
# clones the working tree; the crew member can configure remotes later).
ORIGIN_URL=$(git -C "$RIG_ROOT" remote get-url origin 2>/dev/null || true)
CLONE_SRC=${ORIGIN_URL:-$RIG_ROOT}

# Determine the rig's default branch from origin/HEAD. Falls back to the
# branch currently checked out in the rig if origin/HEAD is unset.
DEFAULT_REF=$(git -C "$RIG_ROOT" symbolic-ref refs/remotes/origin/HEAD 2>/dev/null || true)
if [ -n "$DEFAULT_REF" ]; then
    DEFAULT_BRANCH=${DEFAULT_REF#refs/remotes/origin/}
else
    DEFAULT_BRANCH=$(git -C "$RIG_ROOT" symbolic-ref --short HEAD 2>/dev/null || echo main)
fi

mkdir -p "$(dirname "$TARGET")"

# Stash any pre-existing non-git files (e.g., scratch from a prior abandoned
# attempt) so the clone has an empty target dir. Same pattern as
# worktree-setup.sh.
STAGE=""

merge_stage_entry() (
    SRC="$1"
    DST="$2"
    if [ -d "$SRC" ]; then
        mkdir -p "$DST"
        for ENTRY in "$SRC"/.[!.]* "$SRC"/..?* "$SRC"/*; do
            [ -e "$ENTRY" ] || continue
            merge_stage_entry "$ENTRY" "$DST/$(basename "$ENTRY")"
        done
        rmdir "$SRC" 2>/dev/null || true
        exit 0
    fi
    if [ -e "$DST" ]; then
        exit 0
    fi
    mv "$SRC" "$DST"
)

restore_stage() {
    [ -n "$STAGE" ] || return 0
    mkdir -p "$TARGET"
    for ENTRY in "$STAGE"/.[!.]* "$STAGE"/..?* "$STAGE"/*; do
        [ -e "$ENTRY" ] || continue
        merge_stage_entry "$ENTRY" "$TARGET/$(basename "$ENTRY")"
    done
    rmdir "$STAGE" 2>/dev/null || true
    STAGE=""
}

if [ -d "$TARGET" ] && [ "$(find "$TARGET" -mindepth 1 -maxdepth 1 | head -n 1)" ]; then
    STAGE=$(mktemp -d "$(dirname "$TARGET")/.gascity-clone-stage.XXXXXX")
    find "$TARGET" -mindepth 1 -maxdepth 1 -exec mv {} "$STAGE"/ \;
    trap 'restore_stage' EXIT HUP INT TERM
fi

rmdir "$TARGET" 2>/dev/null || true

# Refresh the default branch from origin so the clone lands on the current tip
# (mirrors worktree-setup.sh; prevents the same "feature branches carry
# already-merged commits" problem when the local rig is stale).
if [ -n "$ORIGIN_URL" ] && [ -n "$DEFAULT_REF" ]; then
    git -C "$RIG_ROOT" fetch origin "$DEFAULT_BRANCH" >/dev/null 2>&1 || true
fi

# Clone with --reference against the rig repo so the new clone shares the
# rig's object store. Drops the disk cost from "full repo" to "alternates
# pointer" and makes the clone fast even on large histories. --dissociate is
# deliberately omitted: keeping the alternate active is fine for a long-lived
# workspace where the rig stays around.
CLONE_CMD="git clone --reference $RIG_ROOT --branch $DEFAULT_BRANCH $CLONE_SRC $TARGET"
if ! GIT_LFS_SKIP_SMUDGE=1 $CLONE_CMD >/dev/null 2>&1; then
    # Retry without --branch in case the default branch doesn't exist yet
    # (new/empty repo) — fall back to whatever clone gives us.
    if ! GIT_LFS_SKIP_SMUDGE=1 git clone --reference "$RIG_ROOT" "$CLONE_SRC" "$TARGET" >/dev/null 2>&1; then
        # Last resort: clone without --reference. Costs more disk but keeps
        # the agent unblocked if the rig's object DB is unhealthy.
        if ! GIT_LFS_SKIP_SMUDGE=1 git clone "$CLONE_SRC" "$TARGET"; then
            echo "clone-setup: failed to clone $CLONE_SRC into $TARGET" >&2
            restore_stage
            exit 1
        fi
    fi
fi

if [ -n "$STAGE" ]; then
    for ENTRY in "$STAGE"/.[!.]* "$STAGE"/..?* "$STAGE"/*; do
        [ -e "$ENTRY" ] || continue
        merge_stage_entry "$ENTRY" "$TARGET/$(basename "$ENTRY")"
    done
    rm -rf "$STAGE"
    STAGE=""
fi
trap - EXIT HUP INT TERM

# Bead redirect: project issues filed from this clone go through the rig's
# bead store, same as polecat worktrees. Mail still routes to town beads via
# the standard prefix lookup.
mkdir -p "$TARGET/.beads"
echo "$RIG_ROOT/.beads" > "$TARGET/.beads/redirect"

# Submodule init (best-effort; some rigs use submodules, most don't).
git -C "$TARGET" submodule init 2>/dev/null || true

# Local excludes — same set as worktree-setup.sh so crew clones don't show
# Gas City runtime files as dirty working-tree noise. Written via git's
# info/exclude so the tracked .gitignore stays untouched.
EXCLUDE=$(git -C "$TARGET" rev-parse --git-path info/exclude)
case "$EXCLUDE" in
    /*) ;;
    *) EXCLUDE="$TARGET/$EXCLUDE" ;;
esac
mkdir -p "$(dirname "$EXCLUDE")"
touch "$EXCLUDE"

MARKER="# Gas City crew clone infrastructure (local excludes)"
if ! grep -qF "$MARKER" "$EXCLUDE" 2>/dev/null; then
    if [ -s "$EXCLUDE" ] && [ "$(tail -c 1 "$EXCLUDE" 2>/dev/null || true)" != "" ]; then
        printf '\n' >> "$EXCLUDE"
    fi
    printf '%s\n' "$MARKER" >> "$EXCLUDE"
fi

append_exclude() {
    PATTERN="$1"
    grep -qxF "$PATTERN" "$EXCLUDE" 2>/dev/null || printf '%s\n' "$PATTERN" >> "$EXCLUDE"
}

append_exclude ".beads/redirect"
append_exclude ".beads/hooks/"
append_exclude ".beads/formulas/"
append_exclude ".runtime/"
append_exclude ".logs/"
append_exclude "worktrees/"
append_exclude "__pycache__/"
append_exclude ".claude/"
append_exclude ".codex/"
append_exclude ".gemini/"
append_exclude ".opencode/"
append_exclude ".github/hooks/"
append_exclude ".github/copilot-instructions.md"
append_exclude "state.json"

exit 0
