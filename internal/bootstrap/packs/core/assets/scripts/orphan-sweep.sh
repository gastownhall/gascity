#!/usr/bin/env bash
# orphan-sweep — reset beads assigned to dead agents.
#
# Replaces the deacon patrol town-orphan-sweep step. Cross-references
# in-progress beads against all known agents. Beads assigned to agents
# that don't exist in ANY rig get reset to open/unassigned so they are
# routable again.
#
# A claim is released only when this city can vouch for its holder: the
# identity resolves against this city's roster, or one of this city's own
# session records (open or closed) answers to it. `gc agent is-foreign`
# answers that question with the Go foreign-identity gate. A claim it cannot
# vouch for -- another city's agent on a shared rig store, a person, an agent
# whose config and session records are gone -- is never released; it is
# counted, named, and declared as a partial outcome (gastownhall/gascity#5955).
#
# Does NOT do worktree salvage — cities that staff a work-health patrol role
# (e.g. the gastown pack's witness) handle that separately.
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
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/order_outcome.sh"

# Where the recovery summary goes. Cities that staff a coordinator role (e.g.
# the gastown pack's mayor) can point this at it; "human" is the reserved
# recipient alias that resolves in every city, including core-only ones.
ESCALATION_TARGET="${GC_ESCALATION_TARGET:-human}"

# Step 1: Collect in-progress beads from HQ and every rig whose session
# liveness can be determined.
# `gc bd list` without --rig is HQ-scoped from the city cwd, so per-rig
# beads are invisible to a bare query — walk every rig explicitly.
# The scope walk keeps per-rig failure isolation and leaves single-store
# behavior as it was, but `gc bd list` reads one work store and leaves out
# the wisp plane, so the HQ scope also stages the federated read
# (read_federated_claims): class-binding rows on a split city, and wisp-plane
# rows everywhere. `gc session list` is city-wide and every row is judged
# against all liveness reads this pass made, so a rig skipped for a failed
# rig-context liveness read can still contribute rows through the federated
# read, judged with the city's liveness bracket.
# Every exit after collection writes the outcome, so a declared failed read is
# not lost when no claim is left to judge.
TMP=$(mktemp) || exit 0
SESSION_TMP=$(mktemp) || {
    rm -f "$TMP"
    exit 0
}
trap 'rm -f "$TMP" "$SESSION_TMP"' EXIT

RIG_NAMES=""
RIG_LIST=$(gc rig list --json 2>/dev/null) || RIG_LIST=""
if [ -n "$RIG_LIST" ]; then
    RIG_NAMES=$(echo "$RIG_LIST" | jq -r '.rigs[] | select(.hq == false and (.suspended // false) == false) | .name' 2>/dev/null) || RIG_NAMES=""
fi

append_session_list() {
    local session_fetch_tmp
    local row_count
    session_fetch_tmp=$(mktemp) || return 1
    if "$@" >"$session_fetch_tmp" 2>/dev/null; then
        # An exit-0 response that parses to zero session rows carries no
        # liveness evidence at all. A city always has at least the calling
        # session live, so zero rows is never a legitimate steady state --
        # treat it the same as a hard failure so the caller skips this
        # scope's beads instead of staging them with no evidence to check
        # against.
        row_count=$(jq -r '.sessions // [] | length' "$session_fetch_tmp" 2>/dev/null) || row_count=""
        case "$row_count" in
            '' | *[!0-9]*) row_count=0 ;;
        esac
        if [ "$row_count" -eq 0 ]; then
            rm -f "$session_fetch_tmp"
            return 1
        fi
        cat "$session_fetch_tmp" >>"$SESSION_TMP"
        rm -f "$session_fetch_tmp"
        return 0
    fi
    rm -f "$session_fetch_tmp"
    return 1
}

# read_federated_claims writes every in-progress bead the federated reader
# returns to $1. `gc bd list` answers from one work store: on a city whose
# [storage.classes] serves the graph class from a binding, molecule roots,
# steps and control beads live in that binding and no gc bd scope lists them
# (exit 0, empty), and bd also leaves wisp-plane rows out of a plain list.
# gc ready --status in_progress reads the city store, every serving rig store
# and every class binding across both storage tiers. It fails on a store it
# cannot read rather than answer short; that failure sets FEDERATED_READ_ERROR
# and is declared after the scope walk, never staged as "no claims".
FEDERATED_READ_ERROR=""
read_federated_claims() {
    local out="$1"
    local err_tmp
    local rc=0
    err_tmp=$(mktemp) || {
        FEDERATED_READ_ERROR="gc ready --status in_progress was not run: mktemp failed"
        return 1
    }
    gc ready --status in_progress --json >"$out" 2>"$err_tmp" || rc=$?
    if [ "$rc" -eq 0 ] && jq -e 'type == "array"' "$out" >/dev/null 2>&1; then
        rm -f "$err_tmp"
        return 0
    fi
    local why=""
    why=$(<"$err_tmp") || why=""
    why="${why//$'\t'/ }"
    FEDERATED_READ_ERROR="gc ready --status in_progress failed (exit $rc): ${why//$'\n'/ }"
    rm -f "$err_tmp"
    : >"$out"
    return 1
}

append_hq_scope() {
    local bead_fetch_tmp
    local federated_tmp
    bead_fetch_tmp=$(mktemp) || return 1
    federated_tmp=$(mktemp) || {
        rm -f "$bead_fetch_tmp"
        return 1
    }
    append_session_list gc session list --json || {
        rm -f "$bead_fetch_tmp" "$federated_tmp"
        return 1
    }
    gc bd list --status=in_progress --json --limit=0 2>/dev/null >"$bead_fetch_tmp" || true
    # The city's scope also stages the claims only the federated reader
    # returns, between the same two liveness reads. Its rows overlap the scope
    # reads' rows; the first row staged for an id wins (Step 1c).
    read_federated_claims "$federated_tmp" || true
    append_session_list gc session list --json || {
        rm -f "$bead_fetch_tmp" "$federated_tmp"
        return 1
    }
    cat "$bead_fetch_tmp" "$federated_tmp" >>"$TMP"
    rm -f "$bead_fetch_tmp" "$federated_tmp"
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

# Step 1c: a federated read that did not complete is declared: this pass
# judged only the claims the gc bd scope reads returned, and on a split city
# that leaves out every graph-class claim.
if [ -n "$FEDERATED_READ_ERROR" ]; then
    echo "orphan-sweep: $FEDERATED_READ_ERROR; this pass considered only the claims gc bd list returned scope by scope" >&2
    order_outcome_scope_skipped "every store (gc ready)" "$FEDERATED_READ_ERROR; only the claims gc bd list returned scope by scope were considered"
fi

IN_PROGRESS=$(jq -c -s 'add // [] | reduce .[] as $b ({seen: {}, out: []};
    ($b.id // "") as $id
    | if $id != "" and .seen[$id] then . else .seen[$id] = true | .out += [$b] end) | .out' "$TMP" 2>/dev/null) || IN_PROGRESS="[]"
if [ "$IN_PROGRESS" = "[]" ]; then
    order_outcome_write
    exit 0
fi

# Step 2: Get all known agent identities from resolved config.
# `gc config explain` prints Agent.QualifiedName(), including import binding
# and rig scope. Fall back to the older config-show parser for older binaries.
AGENTS=$(gc config explain 2>/dev/null | awk '/^Agent: /{print $2}') || AGENTS=""
if [ -z "$AGENTS" ]; then
    AGENTS=$(gc config show 2>/dev/null | awk '/^\[\[agent\]\]/{a=1} a && /^[[:space:]]*name[[:space:]]*=/{print; a=0}' | sed 's/.*=[[:space:]]*"\(.*\)"/\1/') || {
        order_outcome_write
        exit 0
    }
fi
if [ -z "$AGENTS" ]; then
    order_outcome_write
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
    [ .[] | .sessions[]?
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
    ] | unique | .[]
' "$SESSION_TMP" 2>/dev/null) || {
    order_outcome_write
    exit 0
}

agent_exists() {
    local candidate="$1"
    [ -n "$candidate" ] && grep -Fxq -- "$candidate" <<<"$AGENTS"
}

live_session_match() {
    local candidate="$1"
    [ -n "$candidate" ] && [ -n "$LIVE_SESSION_IDS" ] \
        && grep -Fxq -- "$candidate" <<<"$LIVE_SESSION_IDS"
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

    # The stamped gc.session_name is deliberately not a candidate: a pool
    # claim stamps the runtime name "<template>-<session bead id>"
    # (PoolSessionName), which no bead answers to. It is judged separately,
    # after the bead-id candidates, in session_bead_shows_live_assignment.
    printf '%s\n' "$work_json" | jq -r "$first_bead_jq | [
        .metadata[\"gc.session_id\"],
        .metadata[\"gc.session_bead_id\"],
        .metadata[\"session_id\"]
    ] | .[]? | select(. != null and . != \"\")" 2>/dev/null || true

    printf '%s\n' "$assignee" | grep -Eo '[[:alnum:]]+-wisp-[[:alnum:]][[:alnum:]-]*$' || true
}

# The work bead's stamped gc.session_name, empty when the bead carries none.
work_bead_session_name() {
    printf '%s\n' "$1" | jq -r "$first_bead_jq | .metadata[\"gc.session_name\"] // empty" 2>/dev/null
}

# The id of a live session record answering to this claim, or empty: the bead
# is a session bead, is not closed, orphaned, failed-create or flagged closed,
# and one of its identities matches the assignee. The CLI does not expose
# every Go-side identity field, so the fail-safe direction is to treat
# candidate liveness as sufficient.
session_bead_live_match() {
    printf '%s\n' "$1" | jq -r --arg assignee "$2" --arg session_id "$3" "
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
    " 2>/dev/null
}

# The id of a terminal session record on file at this candidate, or empty:
# the bead read back carries the candidate's id, is a session bead, and the
# same conditions that keep a live session live mark it closed, orphaned,
# failed-create or flagged closed. Only such a record can establish a
# holder's death; an open-but-unmatched or non-session bead cannot.
terminal_session_bead_id() {
    printf '%s\n' "$1" | jq -r --arg session_id "$2" "
        $first_bead_jq
        | select((.issue_type // \"\") == \"session\" and (.id // \"\") == \$session_id)
        | select(
            ((.status // \"\") == \"closed\")
            or (((.metadata.state // \"\") | ascii_downcase) == \"closed\")
            or (((.metadata.state // \"\") | ascii_downcase) == \"orphaned\")
            or (((.metadata.state // \"\") | ascii_downcase) == \"failed-create\")
            or (((.metadata.state // \"\") | ascii_downcase) == \"failed_create\")
            or (((.metadata.closed // \"\") | tostring | ascii_downcase) == \"true\")
        )
        | .id // empty
    " 2>/dev/null
}

# A pool seat's runtime name comes from CONFIG, not from a session:
# poolRuntimeSessionName is SanitizeQualifiedNameForSession("<agent>[-<slot>]")
# plus the "-pool" suffix (cmd/gc/session_name_lookup.go). That sanitizer
# encodes "/" as "--" (rig-scope) and "." as "__" (pack-qualified city-scope),
# so reconstructing seat names from $AGENTS recognizes both encodings without
# guessing from the separator alone — an ephemeral session name carries a
# per-session token ("...-gc-<token>") and never reconstructs this way, so it
# still resets when it goes missing.
assignee_is_configured_pool_seat() {
    local name="$1"
    case "$name" in
        *-pool) ;;
        *) return 1 ;;
    esac
    [ -n "$AGENTS" ] || return 1

    local seat="${name%-pool}"
    local slotless="${seat%-[0-9]*}"
    local cfg_agent sanitized
    while IFS= read -r cfg_agent; do
        [ -z "$cfg_agent" ] && continue
        sanitized="${cfg_agent//\//--}"
        sanitized="${sanitized//./__}"
        if [ "$sanitized" = "$seat" ] || [ "$sanitized" = "$slotless" ]; then
            return 0
        fi
    done <<<"$AGENTS"
    return 1
}

session_probe_failure_is_unverifiable() {
    local session_id="$1"
    local assignee="$2"

    [ "$session_id" != "$assignee" ] && return 0
    [[ "$session_id" == mc-* ]] && return 0
    # A session-name-shaped self-probe (e.g. "beads--deployer-pool") has
    # nothing else resolvable to ask about -- bead ids never contain a
    # double dash, so this shape means the probe could not be performed,
    # not that the candidate resolved and came back dead.
    [[ "$session_id" == *--* ]] && return 0
    # Same reasoning for a city-scope pool seat, whose name encodes "." as
    # "__" and so carries no double dash. Recognizing only the double-dash
    # form released live city-scope seats mid-work (ga-dei7xx).
    assignee_is_configured_pool_seat "$session_id" && return 0
    return 1
}

session_bead_shows_live_assignment() {
    local assignee="$1"
    local work_json="$2"
    local session_id
    local session_json
    local live_match
    local terminal_id
    local probe_failed=0
    local probed_ids=""
    local terminal_ids=""
    local stamped_name
    local name_covered

    while IFS= read -r session_id; do
        [ -n "$session_id" ] || continue
        probed_ids="${probed_ids}${session_id}"$'\n'
        if ! session_json=$(gc bd show "$session_id" --json 2>/dev/null); then
            if session_probe_failure_is_unverifiable "$session_id" "$assignee"; then
                probe_failed=1
            fi
            continue
        fi
        # Any live session bead at a probed candidate preserves the work.
        if ! live_match=$(session_bead_live_match "$session_json" "$assignee" "$session_id"); then
            probe_failed=1
            continue
        fi
        if [ -n "$live_match" ]; then
            return 0
        fi
        terminal_id=$(terminal_session_bead_id "$session_json" "$session_id") || terminal_id=""
        if [ -n "$terminal_id" ]; then
            terminal_ids="${terminal_ids}${terminal_id}"$'\n'
        fi
    done < <(session_bead_candidates "$assignee" "$work_json")

    # The stamped gc.session_name is a runtime name, "<template>-<session
    # bead id>", not a bead id: probing it as one always failed, which
    # counted the claim as unverifiable even after the session bead itself
    # read back closed (#7686). Judge it only now, against what the bead-id
    # candidates actually read.
    stamped_name=$(work_bead_session_name "$work_json") || stamped_name=""
    if [ -n "$stamped_name" ]; then
        name_covered=0
        # A value the loop already probed stands as probed: its read and its
        # classification depend only on the value and the assignee, so
        # probing the same string again adds nothing.
        if [ -n "$probed_ids" ] && grep -Fxq -- "$stamped_name" <<<"$probed_ids"; then
            name_covered=1
        else
            # The runtime name of a terminal session bead the loop read: the
            # name adds neither liveness nor unverifiability, and the session
            # record behind it has already been read.
            while IFS= read -r terminal_id; do
                [ -n "$terminal_id" ] || continue
                if [[ "$stamped_name" == *-"$terminal_id" ]]; then
                    name_covered=1
                    break
                fi
            done <<<"$terminal_ids"
        fi
        if [ "$name_covered" -eq 0 ]; then
            # Anything else keeps today's rules: a live session bead under
            # the name keeps the claim (a legacy pool seat whose session bead
            # id is its runtime name), and a failed read is unverifiable.
            if ! session_json=$(gc bd show "$stamped_name" --json 2>/dev/null); then
                if session_probe_failure_is_unverifiable "$stamped_name" "$assignee"; then
                    probe_failed=1
                fi
            elif ! live_match=$(session_bead_live_match "$session_json" "$assignee" "$stamped_name"); then
                probe_failed=1
            elif [ -n "$live_match" ]; then
                return 0
            fi
        fi
    fi

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
        released)
            if ! gc bd update "$bead_id" --append-notes "orphan-sweep: reset from assignee $expected_assignee -- no live session matched" >/dev/null 2>&1; then
                echo "orphan-sweep: failed to record cause note on $bead_id" >&2
            fi
            return 0
            ;;
        skipped) return 2 ;;
        *) return 1 ;;
    esac
}

# Step 3: Find orphaned beads (assigned to non-existent agents).
# Pool instances use names like "worker-3"; strip the -N suffix to match
# the template name from config.
is_known_agent() {
    local name="$1"
    # The human operator. "human" is the canonical operator alias across gc
    # (mail alias `human`, `gc bd human <id>`), and beads assigned to the
    # operator are action items for a person, not agent work. The operator
    # is not a configured agent and never has a session — but is also never
    # a dead agent. Without this guard the sweep resets any in_progress
    # human-assigned bead to open/unassigned, silently wiping the operator's
    # claim. Exact match only: agents that merely start with "human" still
    # resolve through the normal paths below.
    if [ "$name" = "human" ]; then return 0; fi
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

# Step 3a: the foreign-identity gate (gastownhall/gascity#5955).
#
# On a SHARED rig store this sweep sees claims held by another city's agents.
# Their session records live in that city's own unfederated HQ, so the liveness
# checks below find nothing for them and would release live work -- two cities
# stripped each other's P0 claims this way within seconds. A claim whose holder
# is neither known nor live is therefore released only when this city can
# vouch for the holder: `gc agent is-foreign` exits 0 when the identity
# resolves against this city's roster or one of its own session records, open
# or closed, answers to it. Exit 1 (foreign) and any other exit (the gate could
# not answer) both PROTECT the claim: it is not probed, not released, and is
# counted and named below. The verdict is asked once per identity per sweep,
# ahead of revalidation and liveness probes, as in the Go pool sweep.
#
# The skip has to be loud: a decommissioned local agent whose session records
# are gone is indistinguishable from a foreign one, and a silent skip would
# turn a loud cross-city bug into a quiet local leak.
#
# Each gate call opens the city's store (seconds on bd/Dolt), and foreign
# claims are never released, so they are re-asked every sweep. The gate gets
# GC_ORPHAN_SWEEP_GATE_BUDGET_SECONDS (default 120, well under the order's 4m
# timeout) per sweep; once it is spent no identity is asked about, and
# every claim whose holder was not asked is protected like one the gate could
# not answer. Where timeout(1) exists, an ask still running when the budget
# runs out is abandoned and its claim protected, so one store-open stuck
# behind an unresponsive server cannot run the sweep into the order timeout,
# whose kill would report nothing this sweep protected. Without timeout(1)
# (stock macOS) the budget bounds only when asks start. Identities are asked
# in sorted order rotated from a random start, so a backlog of foreign
# identities cannot starve the same local dead claim of a gate answer on
# every sweep.
#
# The budget is a whole number of seconds that bash arithmetic can hold, read
# in base 10: bash reads a leading zero as octal and rejects "08" outright,
# which would leave the sweep no deadline at all. Anything else gets the
# default, never no budget.
GATE_BUDGET_SECONDS="${GC_ORPHAN_SWEEP_GATE_BUDGET_SECONDS:-120}"
case "$GATE_BUDGET_SECONDS" in '' | *[!0-9]*) GATE_BUDGET_SECONDS=120 ;; esac
[ "${#GATE_BUDGET_SECONDS}" -le 18 ] || GATE_BUDGET_SECONDS=120
GATE_BUDGET_SECONDS=$((10#$GATE_BUDGET_SECONDS))
GATE_DEADLINE=""
GATE_BUDGET_SKIPPED=0
GATE_LOCAL=""
GATE_FOREIGN=""
GATE_UNKNOWN=""
GATE_VERDICT=""
GATE_REASONS=""
PROTECTED=0
PROTECTED_ROWS=""
PROTECTED_ID_SAMPLE_LIMIT=5

# gate_list_has never matches an empty candidate: every memo list ends in a
# newline, so the here-string carries an empty last line that grep -Fx would
# match, vouching for an identity the gate was never asked about.
gate_list_has() {
    local list="$1"
    local candidate="$2"
    [ -n "$candidate" ] && [ -n "$list" ] && grep -Fxq -- "$candidate" <<<"$list"
}

# claim_gate_verdict sets GATE_VERDICT to local, foreign or unknown for an
# assignee. It must be called directly, not in $(...), so its memo survives.
claim_gate_verdict() {
    local identity="$1"
    local verdict_json=""
    local rc=0
    local answered=""
    local reason=""
    local remaining=0
    local bounded=""
    if gate_list_has "$GATE_LOCAL" "$identity"; then GATE_VERDICT=local; return 0; fi
    if gate_list_has "$GATE_FOREIGN" "$identity"; then GATE_VERDICT=foreign; return 0; fi
    if gate_list_has "$GATE_UNKNOWN" "$identity"; then GATE_VERDICT=unknown; return 0; fi
    # Read the clock once: the ask is bounded by the remainder this check
    # passed, and a second read could find it spent -- `timeout 0` disables
    # the bound instead of enforcing it.
    remaining=$((GATE_DEADLINE - SECONDS))
    if [ "$remaining" -le 0 ]; then
        GATE_VERDICT=unknown
        GATE_UNKNOWN="${GATE_UNKNOWN}${identity}"$'\n'
        GATE_BUDGET_SKIPPED=$((GATE_BUDGET_SKIPPED + 1))
        gate_record_reason "$identity" "the foreign-identity gate budget of ${GATE_BUDGET_SECONDS}s exhausted; not asked this pass"
        return 0
    fi
    if command -v timeout >/dev/null 2>&1; then
        bounded=1
        verdict_json=$(timeout "$remaining" gc agent is-foreign --json -- "$identity" 2>/dev/null) || rc=$?
    else
        verdict_json=$(gc agent is-foreign --json -- "$identity" 2>/dev/null) || rc=$?
    fi
    # The exit code is the verdict, and it must agree with the JSON verdict:
    # a missing verb, a crash, or unparseable output reads as "could not
    # answer" and protects the claim -- never as local.
    answered=$(jq -r 'select(type == "object") | .verdict // empty' <<<"$verdict_json" 2>/dev/null) || answered=""
    reason=$(jq -r 'select(type == "object") | [.reason, .detail] | map(select(. != null and . != "")) | join(": ")' <<<"$verdict_json" 2>/dev/null) || reason=""
    case "$rc:$answered" in
        0:local)
            GATE_VERDICT=local
            GATE_LOCAL="${GATE_LOCAL}${identity}"$'\n'
            return 0
            ;;
        1:foreign)
            GATE_VERDICT=foreign
            GATE_FOREIGN="${GATE_FOREIGN}${identity}"$'\n'
            ;;
        *)
            GATE_VERDICT=unknown
            GATE_UNKNOWN="${GATE_UNKNOWN}${identity}"$'\n'
            if [ -n "$bounded" ] && [ "$rc" -eq 124 ]; then
                # Whatever it printed before the bound is not an answer: only
                # an exit code that agrees with it is.
                reason="the foreign-identity gate did not finish in the ${remaining}s left of its ${GATE_BUDGET_SECONDS}s budget and was abandoned"
            else
                reason="the foreign-identity gate could not answer (exit $rc)${reason:+: $reason}"
            fi
            ;;
    esac
    gate_record_reason "$identity" "$reason"
    return 0
}

# gate_record_reason remembers why an identity was protected, when there is a
# reason to give.
gate_record_reason() {
    local identity="$1"
    local reason="$2"
    [ -n "$reason" ] || return 0
    # Keyed the way record_protected_claim spools the identity.
    local key="${identity//$'\t'/ }"
    reason="${reason//$'\t'/ }"
    GATE_REASONS="${GATE_REASONS}${key//$'\n'/ }"$'\t'"${reason//$'\n'/ }"$'\n'
}

# record_protected_claim spools one protected claim. Identity and bead are read
# off beads another city may have written: tabs and newlines are neutralized so
# they cannot re-key the grouping or forge a row in the summary.
record_protected_claim() {
    local identity="${1//$'\t'/ }"
    local bead="${2//$'\t'/ }"
    identity="${identity//$'\n'/ }"
    bead="${bead//$'\n'/ }"
    PROTECTED=$((PROTECTED + 1))
    PROTECTED_ROWS="${PROTECTED_ROWS}${identity}"$'\t'"${bead}"$'\n'
    return 0
}

# protected_claims_json groups the spool by identity, sorted, with each
# identity's gate reason when the gate gave one.
protected_claims_json() {
    jq -R -s -c --arg reasons "$GATE_REASONS" '
        ($reasons | split("\n") | map(select(length > 0) | split("\t") | {key: .[0], value: (.[1] // "")}) | from_entries) as $why
        | split("\n") | map(select(length > 0) | split("\t") | {identity: .[0], bead: (.[1] // "")})
        | group_by(.identity)
        | map({identity: .[0].identity, beads: map(.bead), reason: ($why[.[0].identity] // "")})
    ' <<<"$PROTECTED_ROWS"
}

# The claims to consider, one "<bead>\t<assignee>" row each: those whose
# holder neither the roster nor a live session recognizes. Each claim is
# classified once, here; a recognized holder's claim is never released, so it
# needs no gate answer and no probe.
CLAIM_ROWS=$(echo "$IN_PROGRESS" | jq -r '.[] | select(.assignee != null and .assignee != "") | "\(.id)\t\(.assignee)"' 2>/dev/null) || CLAIM_ROWS=""
UNRECOGNIZED_ROWS=""
if [ -n "$CLAIM_ROWS" ]; then
    UNRECOGNIZED_ROWS=$(while IFS=$'\t' read -r _bead _assignee; do
        is_known_agent "$_assignee" || printf '%s\t%s\n' "$_bead" "$_assignee"
    done <<<"$CLAIM_ROWS")
fi

# Ask the gate about every distinct unrecognized claimant, before any per-bead
# read, in sorted order rotated from a random start. The sweep below reads the memo;
# an identity the budget did not reach is protected there.
UNRECOGNIZED=()
if [ -n "$UNRECOGNIZED_ROWS" ]; then
    while IFS= read -r _identity; do
        [ -n "$_identity" ] && UNRECOGNIZED+=("$_identity")
    done < <(while IFS=$'\t' read -r _bead _assignee; do
        printf '%s\n' "$_assignee"
    done <<<"$UNRECOGNIZED_ROWS" | jq -R -s -r 'split("\n") | map(select(length > 0)) | unique | .[]' 2>/dev/null)
fi
_gate_offset=0
if [ "${#UNRECOGNIZED[@]}" -gt 0 ]; then
    # A uniform random start (no state file) gives every claimant the same
    # chance to be asked each sweep, whatever the order's run cadence.
    _gate_offset=$(((RANDOM * 32768 + RANDOM) % ${#UNRECOGNIZED[@]}))
fi
# The budget runs from the first ask: classifying a large sweep's claims is not
# gate time, and must not spend the budget before any claimant is asked.
GATE_DEADLINE=$((SECONDS + GATE_BUDGET_SECONDS))
for ((_i = 0; _i < ${#UNRECOGNIZED[@]}; _i++)); do
    claim_gate_verdict "${UNRECOGNIZED[$(((_gate_offset + _i) % ${#UNRECOGNIZED[@]}))]}"
done

ORPHANED=0
UNVERIFIABLE=0
# Count of undeliverable summaries. Load-bearing for the exit code below.
FAILED=0
# Process substitution (not a pipe) keeps the loop body in the parent
# shell so $ORPHANED survives for the summary message below.
while IFS=$'\t' read -r bead_id assignee; do
    claim_gate_verdict "$assignee"
    if [ "$GATE_VERDICT" != "local" ]; then
        record_protected_claim "$assignee" "$bead_id"
        continue
    fi
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
done < <([ -z "$UNRECOGNIZED_ROWS" ] || printf '%s\n' "$UNRECOGNIZED_ROWS")

PROTECTED_DETAIL=""
if [ "$PROTECTED" -gt 0 ]; then
    PROTECTED_JSON=$(protected_claims_json) || PROTECTED_JSON=""
    if [ -n "$PROTECTED_JSON" ]; then
        PROTECTED_DETAIL=$(jq -r --argjson limit "$PROTECTED_ID_SAMPLE_LIMIT" --argjson claims "$PROTECTED" '
            "orphan-sweep: protected \(length) foreign/unknown identities this pass (\($claims) claims): "
            + (map("\(.identity | tojson) (\(.beads | length): \(.beads[:$limit] | join(", "))\(if (.beads | length) > $limit then " +\((.beads | length) - $limit) more" else "" end))") | join(", "))
        ' <<<"$PROTECTED_JSON") || PROTECTED_DETAIL=""
        order_outcome_set partial "protected $PROTECTED claim(s) held by identities this city could not vouch for; each scope names the identity and why"
        # Reason first, identity last, joined by a bare tab: the spool holds no
        # tab or newline, so the identity reads back verbatim. @tsv would
        # escape a backslash, and the scope would stop naming the identity.
        while IFS=$'\t' read -r reason scope; do
            [ -n "$scope" ] || continue
            order_outcome_scope_skipped "$scope" "$reason"
        done < <(jq -r --argjson limit "$PROTECTED_ID_SAMPLE_LIMIT" '
            .[] | ["\(.beads | length) claim(s) kept: \(.beads[:$limit] | join(", "))\(if (.beads | length) > $limit then " +\((.beads | length) - $limit) more" else "" end)\(if .reason != "" then " (\(.reason))" else "" end)",
                   .identity]
            | join("\t")' <<<"$PROTECTED_JSON")
    fi
    if [ -z "$PROTECTED_DETAIL" ]; then
        PROTECTED_DETAIL="orphan-sweep: protected $PROTECTED claims this pass, but the per-identity summary is UNAVAILABLE"
        order_outcome_set partial "protected $PROTECTED claim(s) held by identities this city could not vouch for (per-identity summary unavailable)"
    fi
    if [ "$GATE_BUDGET_SKIPPED" -gt 0 ]; then
        PROTECTED_DETAIL="$PROTECTED_DETAIL
orphan-sweep: foreign-identity gate budget of ${GATE_BUDGET_SECONDS}s exhausted; $GATE_BUDGET_SKIPPED identities not asked this pass"
    fi
fi

if [ "$ORPHANED" -gt 0 ] || [ "$UNVERIFIABLE" -gt 0 ] || [ "$PROTECTED" -gt 0 ]; then
    SUMMARY="orphan-sweep: reset $ORPHANED orphaned beads"
    if [ "$UNVERIFIABLE" -gt 0 ]; then
        SUMMARY="$SUMMARY, skipped $UNVERIFIABLE unverifiable"
    fi
    if [ "$PROTECTED" -gt 0 ]; then
        SUMMARY="$SUMMARY, protected $PROTECTED foreign/unknown claims"
    fi
    echo "$SUMMARY"
    if [ -n "$PROTECTED_DETAIL" ]; then
        echo "$PROTECTED_DETAIL"
    fi
    if [ "$ORPHANED" -gt 0 ]; then
        if ! gc mail send "$ESCALATION_TARGET" \
            -s "orphan-sweep: reset $ORPHANED orphaned beads" \
            -m "$SUMMARY${PROTECTED_DETAIL:+
$PROTECTED_DETAIL}

orphan-sweep resets in-progress beads whose assignee has no live session or
known agent, and only once gc agent is-foreign vouches for the assignee as
this city's own; claims it cannot vouch for are kept, and any kept this pass
are listed above. Each reset bead now carries a one-line cause note (see its
history). Repeated resets of the same bead may indicate a stuck or
misidentified live session -- inspect via gc bd show <id> --json." \
            2>/dev/null; then
            # Do not swallow an undeliverable summary — a vanished escalation
            # is invisible. Surfacing it requires a non-zero exit (see below).
            echo "orphan-sweep: could not mail escalation target '$ESCALATION_TARGET': $SUMMARY" >&2
            FAILED=$((FAILED + 1))
        fi
    fi
fi

# Declare protected claims as a partial outcome: the controller logs an exit-0
# run's output nowhere, but turns this declaration into a typed order.skipped
# event naming each protected identity (order_outcome.sh). A no-op when nothing
# was protected or when run by hand without GC_ORDER_OUTCOME_FILE.
order_outcome_write

# Loud-fail: the summary has been printed above, so a non-zero exit now
# surfaces the failure line to the controller log without losing the sweep's
# own output. The controller captures an exec order's combined output but logs
# it only on a non-zero exit (order_dispatch.go), so exit 0 would swallow it
# (gastownhall/gascity#4543).
if [ "$FAILED" -gt 0 ]; then
    echo "orphan-sweep: $FAILED escalation summary(ies) could not be delivered to '$ESCALATION_TARGET' (see above)" >&2
    exit 1
fi
