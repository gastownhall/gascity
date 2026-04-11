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
#
# See lx-qfq5r1 / lx-f2z2ph for the hardening pass — sanity limit on
# in-progress query, flock, bd_safe, trap.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-bd-safe.sh
. "$SCRIPT_DIR/lib-bd-safe.sh"
install_trap
acquire_lock

CITY="${GC_CITY:-.}"

# Step 1: Get all in-progress beads with assignees. The in_progress set
# is small in practice (bounded by concurrent agent count). Cap at 500
# as a sanity limit — if more than that are in-progress something is
# seriously wrong and the script shouldn't try to sweep them all at
# once.
IN_PROGRESS=$(bd_safe list --status=in_progress --json --limit=500 2>/dev/null) || exit 0
if [ -z "$IN_PROGRESS" ] || [ "$IN_PROGRESS" = "[]" ]; then
    exit 0
fi

# Step 2: Get all known agent names (from config, scoped to [[agent]] blocks).
AGENTS=$(gc config show 2>/dev/null | awk '/^\[\[agent\]\]/{a=1} a && /^\s*name\s*=/{print; a=0}' | sed 's/.*=\s*"\(.*\)"/\1/') || exit 0
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
ORPHAN_LINES=$(echo "$IN_PROGRESS" | jq -r '.[] | select(.assignee != null and .assignee != "") | "\(.id)\t\(.assignee)"' 2>/dev/null) || ORPHAN_LINES=""
if [ -n "$ORPHAN_LINES" ]; then
    while IFS=$'\t' read -r bead_id assignee; do
        if ! is_known_agent "$assignee"; then
            bd_safe update "$bead_id" --status=open --assignee="" 2>/dev/null || true
            ORPHANED=$((ORPHANED + 1))
        fi
    done <<<"$ORPHAN_LINES"
fi

if [ "$ORPHANED" -gt 0 ]; then
    echo "orphan-sweep: reset $ORPHANED orphaned beads"
fi
