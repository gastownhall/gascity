#!/bin/sh
# worktree-setup.sh — idempotent git worktree creation for Gas City agents.
#
# Usage: worktree-setup.sh <rig-root> <target-dir> <agent-name> [--sync]
#
# Ensures the target directory is a git worktree of the rig repo. For
# backward compatibility, the older <repo-dir> <agent-name> <city-root>
# signature still works and resolves the target under
# <city-root>/.gc/worktrees/<rig>/<agent-name>.
#
# Called from pre_start in pack configs. Runs before the session is created
# so the agent starts IN the worktree directory.
#
# Base branch: GC_DEFAULT_BRANCH (set by the invoking pre_start, when the pack
# passes the rig's configured default_branch through) wins. It is empty when the
# pack does not pass it — as in this pack, whose agents declare start_command
# only — and for rigs that record no default_branch, so the script falls back to
# probing origin/HEAD and finally to the rig's current HEAD. Fresh worktrees are
# always cut from origin/$BRANCH, never from a possibly-stale local ref.

set -eu

RIG_ROOT="${1:?usage: worktree-setup.sh <rig-root> <target-dir> <agent-name> [--sync]}"
ARG2="${2:?missing target-dir}"
ARG3="${3:?missing agent-name}"

is_path_like() {
    # Legacy mode passes the city path as arg 3. Agent names are validated
    # elsewhere and are not expected to look like filesystem paths.
    case "$1" in
        */*|.*|*:*|*\\*) return 0 ;;
        *) return 1 ;;
    esac
}

if is_path_like "$ARG3"; then
    AGENT="$ARG2"
    CITY="$ARG3"
    RIG=$(basename "$RIG_ROOT")
    WT="$CITY/.gc/worktrees/$RIG/$AGENT"
    SYNC="${4:-}"
else
    WT="$ARG2"
    AGENT="$ARG3"
    SYNC="${4:-}"
fi

AGENT_BRANCH="gc-$AGENT"

# Resolve the rig's mainline branch. The configured value wins; otherwise probe
# origin/HEAD. Empty means "no known base" and callers fall back to local HEAD.
resolve_base_branch() {
    CANDIDATE="${GC_DEFAULT_BRANCH:-}"
    if [ -z "$CANDIDATE" ]; then
        CANDIDATE=$(git -C "$RIG_ROOT" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)
        CANDIDATE="${CANDIDATE#origin/}"
    fi
    printf '%s' "$CANDIDATE"
}

append_exclude() {
    PATTERN="$1"
    grep -qxF "$PATTERN" "$EXCLUDE" 2>/dev/null || printf '%s\n' "$PATTERN" >> "$EXCLUDE"
}

# Idempotent: bead redirect, submodule init, and local excludes. Safe to
# call on every invocation (fresh-create AND pre-existing-worktree), so a
# worktree that already existed before this provisioning was added — or
# whose redirect/excludes were later clobbered — converges on re-run
# instead of staying stuck with whatever it had at creation time.
ensure_worktree_provisioning() {
    # Bead redirect for filesystem beads.
    mkdir -p "$WT/.beads"
    echo "$RIG_ROOT/.beads" > "$WT/.beads/redirect"

    # Submodule init (best-effort).
    git -C "$WT" submodule init 2>/dev/null || true

    # Keep runtime ignores local to git metadata instead of mutating the tracked
    # repository .gitignore.
    EXCLUDE=$(git -C "$WT" rev-parse --git-path info/exclude)
    case "$EXCLUDE" in
        /*) ;;
        *) EXCLUDE="$WT/$EXCLUDE" ;;
    esac
    mkdir -p "$(dirname "$EXCLUDE")"
    touch "$EXCLUDE"

    MARKER="# Gas City worktree infrastructure (local excludes)"
    if ! grep -qF "$MARKER" "$EXCLUDE" 2>/dev/null; then
        if [ -s "$EXCLUDE" ] && [ "$(tail -c 1 "$EXCLUDE" 2>/dev/null || true)" != "" ]; then
            printf '\n' >> "$EXCLUDE"
        fi
        printf '%s\n' "$MARKER" >> "$EXCLUDE"
    fi

    append_exclude ".beads/redirect"
    append_exclude ".beads/hooks/"
    append_exclude ".beads/formulas/"
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
}

# Re-anchor a reused worktree onto the base branch, but only when doing so is
# provably lossless: the tree has no uncommitted changes AND every commit on
# HEAD is already reachable from origin/$BRANCH. Anything else is left exactly
# as found. The rule is pure git fact — never a guess about whether the work
# "looks done".
reanchor_worktree() {
    BRANCH=$(resolve_base_branch)
    if [ -z "$BRANCH" ]; then
        echo "worktree-setup: left as-is: no base branch resolved"
        return 0
    fi
    if ! git -C "$WT" fetch origin "$BRANCH" 2>/dev/null; then
        echo "worktree-setup: left as-is: fetch of origin/$BRANCH failed"
        return 0
    fi
    if [ -n "$(git -C "$WT" status --porcelain)" ]; then
        echo "worktree-setup: left as-is: dirty"
        return 0
    fi
    # The clean+ancestor proofs below are about HEAD, but checkout -B moves the
    # AGENT_BRANCH ref; only when HEAD IS that branch is the reset provably
    # lossless (a detached or side-branch HEAD can pass both checks while the
    # agent branch still points at unpushed commits).
    if [ "$(git -C "$WT" symbolic-ref --quiet --short HEAD)" != "$AGENT_BRANCH" ]; then
        echo "worktree-setup: left as-is: not on $AGENT_BRANCH"
        return 0
    fi
    if ! git -C "$WT" merge-base --is-ancestor HEAD "origin/$BRANCH"; then
        echo "worktree-setup: left as-is: unmerged commits"
        return 0
    fi
    if git -C "$WT" checkout -B "$AGENT_BRANCH" "origin/$BRANCH"; then
        echo "worktree-setup: re-anchored $AGENT_BRANCH on origin/$BRANCH"
    else
        echo "worktree-setup: left as-is: checkout of origin/$BRANCH failed"
    fi
}

# Idempotent: skip if worktree already exists.
if [ -d "$WT/.git" ] || [ -f "$WT/.git" ]; then
    ensure_worktree_provisioning
    if [ "$SYNC" = "--sync" ]; then
        reanchor_worktree
    fi
    exit 0
fi

mkdir -p "$(dirname "$WT")"

STAGE=""

merge_stage_entry() {
    SRC="$1"
    DST="$2"

    if [ -d "$SRC" ]; then
        mkdir -p "$DST"
        for ENTRY in "$SRC"/.[!.]* "$SRC"/..?* "$SRC"/*; do
            [ -e "$ENTRY" ] || continue
            merge_stage_entry "$ENTRY" "$DST/$(basename "$ENTRY")"
        done
        rmdir "$SRC" 2>/dev/null || true
        return 0
    fi

    if [ -e "$DST" ]; then
        return 0
    fi
    mv "$SRC" "$DST"
}

restore_stage() {
    [ -n "$STAGE" ] || return 0
    mkdir -p "$WT"
    for ENTRY in "$STAGE"/.[!.]* "$STAGE"/..?* "$STAGE"/*; do
        [ -e "$ENTRY" ] || continue
        merge_stage_entry "$ENTRY" "$WT/$(basename "$ENTRY")"
    done
    rmdir "$STAGE" 2>/dev/null || true
    STAGE=""
}

if [ -d "$WT" ] && [ "$(find "$WT" -mindepth 1 -maxdepth 1 | head -n 1)" ]; then
    STAGE=$(mktemp -d "$(dirname "$WT")/.gascity-worktree-stage.XXXXXX")
    find "$WT" -mindepth 1 -maxdepth 1 -exec mv {} "$STAGE"/ \;
    trap 'restore_stage' EXIT HUP INT TERM
fi

rmdir "$WT" 2>/dev/null || true

create_worktree() {
    if GIT_LFS_SKIP_SMUDGE=1 git -C "$RIG_ROOT" worktree add "$@"; then
        return 0
    fi
    echo "worktree-setup: failed to create worktree at $WT from $RIG_ROOT (branch $AGENT_BRANCH)" >&2
    restore_stage
    exit 1
}

BRANCH=$(resolve_base_branch)
if [ -n "$BRANCH" ]; then
    git -C "$RIG_ROOT" fetch origin "$BRANCH" 2>/dev/null || true
fi

if git -C "$RIG_ROOT" show-ref --verify --quiet "refs/heads/$AGENT_BRANCH"; then
    # The agent branch already exists and may carry unpushed work; adopt it
    # untouched rather than rewinding it onto the base branch.
    create_worktree "$WT" "$AGENT_BRANCH"
    echo "worktree-setup: adopted existing branch $AGENT_BRANCH"
elif [ -n "$BRANCH" ] && git -C "$RIG_ROOT" rev-parse --verify --quiet "refs/remotes/origin/$BRANCH^{commit}" >/dev/null; then
    create_worktree "$WT" -b "$AGENT_BRANCH" "origin/$BRANCH"
    echo "worktree-setup: created $AGENT_BRANCH from origin/$BRANCH"
else
    # No remote base to anchor on (no origin, or an unfetchable base branch).
    create_worktree "$WT" -b "$AGENT_BRANCH"
    echo "worktree-setup: created $AGENT_BRANCH from rig-root HEAD (no origin/$BRANCH)"
fi

if [ -n "$STAGE" ]; then
    for ENTRY in "$STAGE"/.[!.]* "$STAGE"/..?* "$STAGE"/*; do
        [ -e "$ENTRY" ] || continue
        merge_stage_entry "$ENTRY" "$WT/$(basename "$ENTRY")"
    done
    rm -rf "$STAGE"
    STAGE=""
fi
trap - EXIT HUP INT TERM

ensure_worktree_provisioning

exit 0
