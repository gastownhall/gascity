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

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
case "${BASH_SOURCE[0]}" in
    */*) __SCRIPT_DIR="$(cd "${BASH_SOURCE[0]%/*}" && pwd)" ;;
    *) __SCRIPT_DIR="$(pwd)" ;;
esac
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "orphan-sweep"

# Step 1: Collect in-progress beads from HQ and every rig whose session
# liveness can be determined.
# `gc bd list` without --rig is HQ-scoped from the city cwd, so per-rig
# beads are invisible to a bare query — walk every rig explicitly.
TMP=$(mktemp) || exit 0
SESSION_TMP=$(mktemp) || {
    rm -f "$TMP"
    exit 0
}
trap 'rm -f "$TMP" "$SESSION_TMP"' EXIT

RIG_NAMES=""
RIG_LIST=$(gc rig list --json 2>/dev/null) || RIG_LIST=""
if [ -n "$RIG_LIST" ]; then
    RIG_NAMES=$(echo "$RIG_LIST" | jq -r '.rigs[] | select(.hq == false) | .name' 2>/dev/null) || RIG_NAMES=""
fi

append_session_list() {
    local session_fetch_tmp
    session_fetch_tmp=$(mktemp) || return 1
    if "$@" >"$session_fetch_tmp" 2>/dev/null; then
        cat "$session_fetch_tmp" >>"$SESSION_TMP"
        rm -f "$session_fetch_tmp"
        return 0
    fi
    rm -f "$session_fetch_tmp"
    return 1
}

append_hq_scope() {
    local bead_fetch_tmp
    bead_fetch_tmp=$(mktemp) || return 1
    append_session_list gc session list --json || {
        rm -f "$bead_fetch_tmp"
        return 1
    }
    gc bd list --status=in_progress --json --limit=0 2>/dev/null >"$bead_fetch_tmp" || true
    append_session_list gc session list --json || {
        rm -f "$bead_fetch_tmp"
        return 1
    }
    cat "$bead_fetch_tmp" >>"$TMP"
    rm -f "$bead_fetch_tmp"
}

append_rig_scope() {
    local rig="$1"
    local bead_fetch_tmp
    bead_fetch_tmp=$(mktemp) || return 1
    append_session_list gc --rig "$rig" session list --json || {
        rm -f "$bead_fetch_tmp"
        return 1
    }
    gc bd list --rig "$rig" --status=in_progress --json --limit=0 2>/dev/null >"$bead_fetch_tmp" || true
    append_session_list gc --rig "$rig" session list --json || {
        rm -f "$bead_fetch_tmp"
        return 1
    }
    cat "$bead_fetch_tmp" >>"$TMP"
    rm -f "$bead_fetch_tmp"
}

# Step 1b: Get all known live session identities around each bead-list query.
# The second liveness pass closes the session-list-before-bd-list race where a
# newly spawned session can claim work after the first pass but before bd-list.
# HQ liveness is required; per-rig failures only skip that rig's staged bead
# rows so one stale or unreachable rig does not disable cleanup elsewhere.
append_hq_scope || exit 0

while IFS= read -r rig; do
    [ -z "$rig" ] && continue
    if ! append_rig_scope "$rig"; then
        echo "orphan-sweep: skipping rig $rig after session-list failure" >&2
    fi
done <<<"$RIG_NAMES"

IN_PROGRESS=$(jq -c -s 'add // []' "$TMP" 2>/dev/null) || IN_PROGRESS="[]"
if [ "$IN_PROGRESS" = "[]" ]; then
    exit 0
fi

# Step 2: Get all known agent identities from resolved config.
# `gc config explain` prints Agent.QualifiedName(), including import binding
# and rig scope. Fall back to the older config-show parser for older binaries.
AGENTS=$(gc config explain 2>/dev/null | awk '/^Agent: /{print $2}') || AGENTS=""
if [ -z "$AGENTS" ]; then
    AGENTS=$(gc config show 2>/dev/null | awk '/^\[\[agent\]\]/{a=1} a && /^[[:space:]]*name[[:space:]]*=/{print; a=0}' | sed 's/.*=[[:space:]]*"\(.*\)"/\1/') || exit 0
fi
if [ -z "$AGENTS" ]; then
    exit 0
fi

# Step 2b: Parse identities of every session row that `gc session list --json`
# reports as open so that pool-spawned ephemeral assignees (e.g.
# gastown__polekitten-gc-q9j0om) are treated as known. The Go-side
# releaseOrphanedPoolAssignments path validates these from session beads via
# liveOpenSessionAssignmentExists, but this shell sweep only has the CLI JSON
# contract available. That means it protects the exposed wire identities below;
# it cannot see bead-only fields such as configured_named_identity or
# alias_history unless the CLI starts exporting them.
#
# The default CLI path already omits closed sessions. The closed/state guards
# below keep explicit or future session-list producers from making terminal
# rows live while preserving any non-closed row the CLI reports.
#
# This shell sweep ran without a live-session guard before ga-nvx: an ephemeral
# assignee whose template-stripped form did not match any agent name was
# incorrectly reset, racing against active polekitten work and producing a
# false-orphan loop.
# Two bugs the chronic strip pattern (gastownhall/gascity#2363) revealed:
# (1) The JSON shape is {"sessions":[...], "summary":..., "filters":..., "schema_version":...},
#     so `.[]` iterated four top-level scalar keys instead of session objects.
# (2) Field names vary by runtime/API path. The current CLI emits snake_case
#     (.closed/.id/.session_name/.alias/.agent_name); PascalCase is accepted
#     only as forward-compatible hardening so a casing change cannot make
#     LIVE_SESSION_IDS empty and strip active pool claims.
LIVE_SESSION_IDS=$(jq -r -s '
    def pick($snake; $pascal; $default):
      if has($snake) and .[$snake] != null then .[$snake]
      elif has($pascal) and .[$pascal] != null then .[$pascal]
      else $default end;
    .[] | .sessions[]?
    | select(
        (pick("closed"; "Closed"; false) == false)
        and ((pick("state"; "State"; "") | ascii_downcase) != "closed")
      )
    | [
        pick("id"; "ID"; null),
        pick("session_name"; "SessionName"; null),
        pick("alias"; "Alias"; null),
        pick("agent_name"; "AgentName"; null),
        pick("template"; "Template"; null),
        pick("name"; "Name"; null)
      ]
    | .[]
    | select(. != null and . != "")
' "$SESSION_TMP" 2>/dev/null) || exit 0

agent_exists() {
    local candidate="$1"
    [ -n "$candidate" ] && printf '%s\n' "$AGENTS" | grep -Fxq -- "$candidate"
}

live_session_match() {
    local candidate="$1"
    [ -n "$candidate" ] && [ -n "$LIVE_SESSION_IDS" ] \
        && printf '%s\n' "$LIVE_SESSION_IDS" | grep -Fxq -- "$candidate"
}

CURRENT_BEAD_JSON=""

first_bead_jq='if type == "array" then .[0] else . end'

work_bead_still_resettable() {
    local bead_id="$1"
    local expected_assignee="$2"
    local current_status
    local current_assignee

    CURRENT_BEAD_JSON=$(gc bd show "$bead_id" --json 2>/dev/null) || return 1
    current_status=$(printf '%s\n' "$CURRENT_BEAD_JSON" | jq -r "$first_bead_jq | .status // empty" 2>/dev/null) || return 1
    current_assignee=$(printf '%s\n' "$CURRENT_BEAD_JSON" | jq -r "$first_bead_jq | .assignee // empty" 2>/dev/null) || return 1

    [ "$current_status" = "in_progress" ] || return 2
    [ "$current_assignee" = "$expected_assignee" ] || return 2
}

session_bead_candidates() {
    local assignee="$1"
    local work_json="$2"

    if [ -n "$assignee" ]; then
        printf '%s\n' "$assignee"
    fi

    if [[ "$assignee" == *-mc-* ]]; then
        printf 'mc-%s\n' "${assignee##*-mc-}"
    fi

    printf '%s\n' "$work_json" | jq -r "$first_bead_jq | [
        .metadata[\"gc.session_id\"],
        .metadata[\"gc.session_bead_id\"],
        .metadata[\"session_id\"]
    ] | .[]? | select(. != null and . != \"\")" 2>/dev/null || true

    printf '%s\n' "$assignee" | grep -Eo '[[:alnum:]]+-wisp-[[:alnum:]][[:alnum:]-]*$' || true
}

session_probe_failure_is_unverifiable() {
    local session_id="$1"
    local assignee="$2"

    [ "$session_id" != "$assignee" ] && return 0
    [[ "$session_id" == mc-* ]] && return 0
    return 1
}

session_bead_shows_live_assignment() {
    local assignee="$1"
    local work_json="$2"
    local session_id
    local session_json
    local live_match
    local probe_failed=0

    while IFS= read -r session_id; do
        [ -n "$session_id" ] || continue
        if ! session_json=$(gc bd show "$session_id" --json 2>/dev/null); then
            if session_probe_failure_is_unverifiable "$session_id" "$assignee"; then
                probe_failed=1
            fi
            continue
        fi
        # Any live session bead at a probed candidate preserves the work. The
        # CLI does not expose every Go-side identity field, so the fail-safe
        # direction is to treat candidate liveness as sufficient.
        if ! live_match=$(printf '%s\n' "$session_json" | jq -r --arg assignee "$assignee" --arg session_id "$session_id" "
            $first_bead_jq
            | select((.issue_type // \"\") == \"session\")
            | select((.status // \"\") != \"closed\")
            | ((.metadata.state // \"\") | ascii_downcase) as \$state
            | select(\$state != \"closed\" and \$state != \"orphaned\" and \$state != \"failed-create\" and \$state != \"failed_create\")
            | select(((.metadata.closed // \"\") | tostring | ascii_downcase) != \"true\")
            | select(
                (.id // \"\") == \$assignee
                or (.id // \"\") == \$session_id
                or (.metadata.session_name // \"\") == \$assignee
                or (.metadata.alias // \"\") == \$assignee
                or (.metadata.agent_name // \"\") == \$assignee
                or (\$assignee | contains(\$session_id))
            )
            | .id // empty
        " 2>/dev/null); then
            probe_failed=1
            continue
        fi
        if [ -n "$live_match" ]; then
            return 0
        fi
    done < <(session_bead_candidates "$assignee" "$work_json")

    [ "$probe_failed" -eq 0 ] || return 2
    return 1
}

reset_orphan_if_current() {
    local bead_id="$1"
    local expected_assignee="$2"
    local reset_output
    local reset_state

    reset_output=$(gc bd release-if-current "$bead_id" "$expected_assignee" 2>/dev/null) || return 1
    reset_state=$(printf '%s\n' "$reset_output" | awk 'NF { print $1; exit }')
    case "$reset_state" in
        released) return 0 ;;
        skipped) return 2 ;;
        *) return 1 ;;
    esac
}

# Step 3: Find orphaned beads (assigned to non-existent agents).
# Pool instances use names like "worker-3"; strip the -N suffix to match
# the template name from config.
is_known_agent() {
    local name="$1"
    # Direct match against a configured agent template name.
    if agent_exists "$name"; then return 0; fi
    # Pool instance: strip trailing -<digits> and check template name.
    local base="${name%-[0-9]*}"
    if [ "$base" != "$name" ] && agent_exists "$base"; then return 0; fi
    # City-qualified assignee (gastown.deacon): strip everything through the
    # last dot and re-check. This relies on flattened pack binding chains.
    # Defense-in-depth for older binaries that fall through to `gc config show`
    # and emit unqualified names. Also covers pool patterns like
    # "gastown.dog-3" by re-stripping the -N suffix.
    local short="${name##*.}"
    if [ "$short" != "$name" ]; then
        if agent_exists "$short"; then return 0; fi
        local short_base="${short%-[0-9]*}"
        if [ "$short_base" != "$short" ] && agent_exists "$short_base"; then return 0; fi
    fi
    # Live ephemeral session names like gastown__polekitten-gc-q9j0om won't
    # match any template form — accept them as known when a non-closed session
    # is currently running with a matching ID, SessionName, Alias, or
    # AgentName. Mirrors liveOpenSessionAssignmentExists in the Go path.
    if live_session_match "$name"; then return 0; fi
    # Bare short-form assignee whose canonical agent is LIVE: an assignee like
    # "backend_dev" can be the last-dot segment of a configured qualified agent
    # ("thriva/devpipeline.backend_dev") whose live session is known only by the
    # qualified name. Without this, such an assignee looks like a dead agent and
    # the LIVE owner's in-progress work is reset every cycle. Match the assignee
    # against each configured agent's short form, and accept ONLY when that
    # qualified agent currently has a live session (so genuinely dead-agent work
    # still resets). Mirrors the Go reconciler's multi-identity resolution
    # (sessionBeadAssigneeIdentities / liveOpenSessionAssignmentExists).
    if [ -n "$name" ] && [ -n "$AGENTS" ]; then
        while IFS= read -r _cfg_agent; do
            [ -z "$_cfg_agent" ] && continue
            if [ "${_cfg_agent##*.}" = "$name" ] && live_session_match "$_cfg_agent"; then
                return 0
            fi
        done <<<"$AGENTS"
    fi
    return 1
}

ORPHANED=0
UNVERIFIABLE=0
# Process substitution (not a pipe) keeps the loop body in the parent
# shell so $ORPHANED survives for the summary message below.
while IFS=$'\t' read -r bead_id assignee; do
    if ! is_known_agent "$assignee"; then
        if work_bead_still_resettable "$bead_id" "$assignee"; then
            :
        else
            recheck_status=$?
            if [ "$recheck_status" != "2" ]; then
                UNVERIFIABLE=$((UNVERIFIABLE + 1))
            fi
            continue
        fi
        if session_bead_shows_live_assignment "$assignee" "$CURRENT_BEAD_JSON"; then
            continue
        else
            session_status=$?
            if [ "$session_status" = "2" ]; then
                UNVERIFIABLE=$((UNVERIFIABLE + 1))
                continue
            fi
        fi
        if reset_orphan_if_current "$bead_id" "$assignee"; then
            ORPHANED=$((ORPHANED + 1))
        else
            reset_status=$?
            if [ "$reset_status" != "2" ]; then
                UNVERIFIABLE=$((UNVERIFIABLE + 1))
            fi
        fi
    fi
done < <(echo "$IN_PROGRESS" | jq -r '.[] | select(.assignee != null and .assignee != "") | "\(.id)\t\(.assignee)"' 2>/dev/null)

if [ "$ORPHANED" -gt 0 ] || [ "$UNVERIFIABLE" -gt 0 ]; then
    SUMMARY="orphan-sweep: reset $ORPHANED orphaned beads"
    if [ "$UNVERIFIABLE" -gt 0 ]; then
        SUMMARY="$SUMMARY, skipped $UNVERIFIABLE unverifiable"
    fi
    echo "$SUMMARY"
fi
