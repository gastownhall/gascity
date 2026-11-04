#!/usr/bin/env bash
# rig_namespace_gate.sh — shared helpers for core orders that act on rows of
# a rig with federation = "rig" (a rig whose bead store other cities also
# attach). Source it; it defines functions only.
#
# A guarded rig serves only identities inside its namespace ("<rig>/...")
# and this city's own pool sessions of the rig's agents. `gc agent is-foreign
# --rig-namespace <rig>` decides that; these helpers ask it and read the
# answer fail-closed.
#
# `gc rig list --json` reports `federation` only for a guarded rig. Any value
# other than "" or "city" is read as guarded, like the Go accessor, so an
# unexpected spelling never fails open.

# rig_namespace_city_may_guard succeeds unless gc has told this order that
# the city declares no guarded rig. The controller exports GC_GUARDED_RIGS to
# every exec order: the guarded rigs' names, comma-separated, from the config
# it loaded, and "" when there are none. Set and empty, the order needs no
# rig-namespace check and no `gc rig list` for one (in a large city that read
# takes tens of seconds). Unset, the gc that ran the order does not export it,
# or the script was run by hand: the city may have a guarded rig, so the
# caller reads the rig list and fails closed when it cannot.
rig_namespace_city_may_guard() {
    [ -n "${GC_GUARDED_RIGS+set}" ] || return 0
    local names="${GC_GUARDED_RIGS//[[:space:],]/}"
    [ -n "$names" ]
}

# rig_namespace_rig_list_readable <rig-list-json> succeeds when the rig list
# is what `gc rig list --json` prints when it succeeds: an object with a rigs
# array. An empty answer (the call failed), gc's error envelope and text that
# does not parse fail, so a caller can tell "no rig is guarded" from "this
# run cannot tell".
rig_namespace_rig_list_readable() {
    printf '%s' "${1:-}" | jq -e 'type == "object" and (.rigs | type) == "array"' >/dev/null 2>&1
}

# rig_namespace_guarded_rigs <rig-list-json> prints the names of the guarded,
# non-HQ rigs, one per line. It prints nothing for a rig list it cannot read,
# so a caller that must tell that apart from "no rig is guarded" checks
# rig_namespace_rig_list_readable first.
rig_namespace_guarded_rigs() {
    printf '%s' "${1:-}" | jq -r '
        (.rigs // [])[]
        | select((.hq // false) != true)
        | select(((.federation // "") | tostring | ascii_downcase) as $f | $f != "" and $f != "city")
        | .name // empty' 2>/dev/null || true
}

# rig_namespace_rig_is_guarded <rig-list-json> <rig> succeeds when the named
# rig is guarded. Like rig_namespace_guarded_rigs, it fails for a rig list it
# cannot read.
rig_namespace_rig_is_guarded() {
    local rig="${2:-}"
    [ -n "$rig" ] || return 1
    rig_namespace_guarded_rigs "${1:-}" | grep -Fxq -- "$rig"
}

# rig_namespace_guarded_rig_for_bead <rig-list-json> <bead-id> prints the
# guarded rig whose bead prefix the id carries ("<prefix>-...", compared
# case-insensitively, the longest prefix winning), or nothing. It fails
# (status 2) when the rig list cannot be read (rig_namespace_rig_list_readable),
# so a caller fails closed instead of judging the bead as in no guarded rig.
rig_namespace_guarded_rig_for_bead() {
    local id="${2:-}"
    rig_namespace_rig_list_readable "${1:-}" || return 2
    [ -n "$id" ] || return 0
    printf '%s' "${1:-}" | jq -r --arg id "$id" '
        [ (.rigs // [])[]
          | select((.hq // false) != true)
          | select(((.federation // "") | tostring | ascii_downcase) as $f | $f != "" and $f != "city")
          | select((.prefix // "") != "")
          | select((.prefix | ascii_downcase) as $p | ($id | ascii_downcase) | startswith($p + "-"))
        ]
        | sort_by(.prefix | length) | reverse | .[0].name // empty' 2>/dev/null || return 2
}

# rig_namespace_guarded_rigs_for_row <rig-list-json> <store-rig> <bead-id>
# prints the guarded rigs a row is judged in, one per line: the rig whose
# store the row was read from (store-rig, "" for HQ or unknown) when it is
# guarded, and the guarded rig the bead's prefix maps to when that differs.
# The store the row came from is the authority; the prefix only adds a check.
# It fails when the rig list cannot be read.
rig_namespace_guarded_rigs_for_row() {
    local rigs_json="${1:-}"
    local store_rig="${2:-}"
    local id="${3:-}"
    local by_prefix
    rig_namespace_rig_list_readable "$rigs_json" || return 1
    if [ -n "$store_rig" ] && rig_namespace_rig_is_guarded "$rigs_json" "$store_rig"; then
        printf '%s\n' "$store_rig"
    fi
    by_prefix="$(rig_namespace_guarded_rig_for_bead "$rigs_json" "$id")" || return 1
    if [ -n "$by_prefix" ] && [ "$by_prefix" != "$store_rig" ]; then
        printf '%s\n' "$by_prefix"
    fi
}

# rig_namespace_admits_row <rig-list-json> <store-rig> <bead-id> <identity>
# succeeds when every guarded rig the row is judged in
# (rig_namespace_guarded_rigs_for_row) vouches for identity; a row in no
# guarded rig always succeeds. It fails when those rigs cannot be read.
rig_namespace_admits_row() {
    local rig rigs
    rigs="$(rig_namespace_guarded_rigs_for_row "$1" "$2" "$3")" || return 1
    while IFS= read -r rig; do
        [ -n "$rig" ] || continue
        rig_namespace_admits "$rig" "$4" || return 1
    done <<<"$rigs"
    return 0
}

# rig_namespace_ask <rig> <identity> asks `gc agent is-foreign --rig-namespace`
# and sets RIG_NAMESPACE_VERDICT to local, foreign or unknown and
# RIG_NAMESPACE_REASON to the verdict's reason (and detail). Fail-closed: the
# answer is local only when the exit code is 0, the JSON says local, and it
# echoes rig_namespace. An older gc without the flag exits non-zero (unknown
# flag); a gc that ignored it would echo nothing; both read as unknown.
rig_namespace_ask() {
    local rig="$1"
    local identity="$2"
    local verdict_json=""
    local rc=0
    local answered=""
    local echoed=""
    RIG_NAMESPACE_VERDICT=unknown
    RIG_NAMESPACE_REASON=""
    if [ -z "$rig" ] || [ -z "$identity" ]; then
        RIG_NAMESPACE_REASON="no rig or identity to ask about"
        return 0
    fi
    # stdin is closed so a gc that reads it cannot swallow the caller's
    # next loop line.
    verdict_json=$(gc agent is-foreign --rig-namespace "$rig" --json -- "$identity" </dev/null 2>/dev/null) || rc=$?
    answered=$(jq -r 'select(type == "object") | .verdict // empty' <<<"$verdict_json" 2>/dev/null) || answered=""
    echoed=$(jq -r 'select(type == "object") | .rig_namespace // empty' <<<"$verdict_json" 2>/dev/null) || echoed=""
    RIG_NAMESPACE_REASON=$(jq -r 'select(type == "object") | [.reason, .detail] | map(select(. != null and . != "")) | join(": ")' <<<"$verdict_json" 2>/dev/null) || RIG_NAMESPACE_REASON=""
    case "$rc:$answered" in
        0:local)
            if [ "$echoed" = "$rig" ]; then
                RIG_NAMESPACE_VERDICT=local
            else
                RIG_NAMESPACE_REASON="gc did not acknowledge --rig-namespace"
            fi
            ;;
        1:foreign) RIG_NAMESPACE_VERDICT=foreign ;;
        *) RIG_NAMESPACE_REASON="gc agent is-foreign could not answer (exit $rc)${RIG_NAMESPACE_REASON:+: $RIG_NAMESPACE_REASON}" ;;
    esac
    return 0
}

# rig_namespace_admits <rig> <identity> succeeds only when this city can vouch
# for identity in the rig (rig_namespace_ask answers local).
rig_namespace_admits() {
    rig_namespace_ask "$1" "$2"
    [ "$RIG_NAMESPACE_VERDICT" = "local" ]
}

# rig_namespace_fold <value> prints value under the bead store's actor fold,
# as gc's config.FoldIdentity does: surrounding space trimmed, a run of '.',
# '_' or '-' that is exactly "--" becomes "/", and any other such run becomes
# "_". It fails when jq cannot fold the value.
rig_namespace_fold() {
    jq -nr --arg v "${1:-}" '$v
        | sub("^[[:space:]]+"; "") | sub("[[:space:]]+$"; "")
        | gsub("(?<run>[._-]+)"; if .run == "--" then "/" else "_" end)' 2>/dev/null
}

# rig_namespace_stamp_names_other <rig> <stamp> succeeds when the creation
# stamp (gc.owner_namespace, which a guarded store writes on every row it
# creates) is set and names a namespace other than the rig's own ("<rig>/"
# folded): another city's guard created the row. An empty stamp names no
# one. A stamp that cannot be folded counts as another's (fail closed).
rig_namespace_stamp_names_other() {
    local rig="${1:-}"
    local stamp="${2:-}"
    local folded=""
    local own=""
    folded="$(rig_namespace_fold "$stamp")" || return 0
    [ -n "$folded" ] || return 1
    own="$(rig_namespace_fold "$rig")" || return 0
    [ -n "$own" ] && [ "$folded" = "$own/" ] && return 1
    return 0
}

# rig_namespace_row_stamp_names_other <rig-list-json> <store-rig> <bead-id>
# <stamp> succeeds when the creation stamp names a namespace other than that
# of a guarded rig the row is judged in (rig_namespace_guarded_rigs_for_row):
# another city's guard created the row. A row in no guarded rig fails. It
# succeeds when those rigs cannot be read, so a caller fails closed.
rig_namespace_row_stamp_names_other() {
    local rig rigs
    rigs="$(rig_namespace_guarded_rigs_for_row "$1" "$2" "$3")" || return 0
    while IFS= read -r rig; do
        [ -n "$rig" ] || continue
        rig_namespace_stamp_names_other "$rig" "$4" && return 0
    done <<<"$rigs"
    return 1
}
