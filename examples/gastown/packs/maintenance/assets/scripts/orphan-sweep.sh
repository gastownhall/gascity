#!/usr/bin/env bash
# orphan-sweep — reset beads assigned to dead agents.
#
# Replaces the deacon patrol town-orphan-sweep step. Cross-references
# in-progress beads against all known agents. Beads assigned to agents
# that don't exist in ANY rig get reset to open/unassigned so the rig's
# witness picks them up on its next patrol.
#
# Does NOT do worktree salvage — that's the witness's job.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

CITY="${GC_CITY:-.}"

# Step 1: Collect in-progress beads from HQ and every rig.
# `gc bd list` without --rig is HQ-scoped from the city cwd, so per-rig
# beads are invisible to a bare query — walk every rig explicitly.
TMP=$(mktemp) || exit 0
trap 'rm -f "$TMP"' EXIT

gc bd list --status=in_progress --json --limit=0 2>/dev/null >>"$TMP" || true

RIG_LIST=$(gc rig list --json 2>/dev/null) || RIG_LIST=""
if [ -n "$RIG_LIST" ]; then
    RIG_NAMES=$(echo "$RIG_LIST" | jq -r '.rigs[] | select(.hq == false) | .name' 2>/dev/null) || RIG_NAMES=""
    while IFS= read -r rig; do
        [ -z "$rig" ] && continue
        gc bd list --rig "$rig" --status=in_progress --json --limit=0 2>/dev/null >>"$TMP" || true
    done <<<"$RIG_NAMES"
fi

IN_PROGRESS=$(jq -c -s 'add // []' "$TMP" 2>/dev/null) || IN_PROGRESS="[]"
if [ "$IN_PROGRESS" = "[]" ]; then
    exit 0
fi

# Step 2: Get all known agent identities from resolved config.
# `gc config explain` prints Agent.QualifiedName(), including import binding
# and rig scope. Fall back to the older config-show parser for older binaries.
AGENTS=$(gc config explain 2>/dev/null | awk '/^Agent: /{print $2}') || AGENTS=""
if [ -z "$AGENTS" ]; then
    AGENTS=$(gc config show 2>/dev/null | awk '/^\[\[agent\]\]/{a=1} a && /^\s*name\s*=/{print; a=0}' | sed 's/.*=\s*"\(.*\)"/\1/') || exit 0
fi
if [ -z "$AGENTS" ]; then
    exit 0
fi

# Build a lookup set of known agents.
declare -A KNOWN_AGENTS
while IFS= read -r agent; do
    KNOWN_AGENTS["$agent"]=1
done <<< "$AGENTS"

# Step 3: Find orphaned beads (assigned to non-existent agents).
# Pool instances use names like "worker-3"; strip the -N suffix to match
# the template name from config.
is_known_agent() {
    local name="$1"
    # Direct match.
    if [ -n "${KNOWN_AGENTS[$name]+x}" ]; then return 0; fi
    # Pool instance: strip trailing -<digits> and check template name.
    local base="${name%-[0-9]*}"
    if [ "$base" != "$name" ] && [ -n "${KNOWN_AGENTS[$base]+x}" ]; then return 0; fi
    return 1
}

ORPHANED=0
# Process substitution (not a pipe) keeps the loop body in the parent
# shell so $ORPHANED survives for the summary message below.
while IFS=$'\t' read -r bead_id assignee; do
    if ! is_known_agent "$assignee"; then
        # `gc bd update` auto-resolves the bead's prefix to the right rig
        # store, so HQ and rig beads update in the correct database.
        gc bd update "$bead_id" --status=open --assignee="" 2>/dev/null || true
        ORPHANED=$((ORPHANED + 1))
    fi
done < <(echo "$IN_PROGRESS" | jq -r '.[] | select(.assignee != null and .assignee != "") | "\(.id)\t\(.assignee)"' 2>/dev/null)

if [ "$ORPHANED" -gt 0 ]; then
    echo "orphan-sweep: reset $ORPHANED orphaned beads"
fi
