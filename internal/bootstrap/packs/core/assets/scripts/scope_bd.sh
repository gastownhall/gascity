#!/usr/bin/env bash
# scope_bd.sh — the bead-store front door for the core maintenance orders.
#
# Sourced by reaper.sh and jsonl-export.sh. Every bead scope (the city and each
# rig) is reached only through `gc bd --city <city> [--rig <rig>] <verb>`; bd
# picks the transport (proxied, direct server, embedded) for that scope. The
# maintenance scripts therefore carry no Dolt ports, no SQL connections of
# their own and no topology branches.
#
# Mutations go through gc bd verbs only (close, update, purge, prune, delete),
# which own their commits in every bd mode. `gc bd sql` is used for read-only
# selection where no verb filter exists, and scope_sql_read refuses anything
# that is not a read.
#
# Caller contract: CITY_ABS is the absolute city path. The caller selects a
# scope with scope_select before calling scope_bd / scope_sql_read.

# The SCOPE_* variables are this file's output for the sourcing script.
# shellcheck disable=SC2034

SCOPE_KIND=""
SCOPE_RIG=""
SCOPE_LABEL=""
SCOPE_DB=""
SCOPE_LAST_ERROR=""
# SCOPE_NOT_BD is 1 when gc reports that the scope's beads provider is not bd
# (a file- or exec-backed store): there is nothing for these orders to do
# there, which is not a failure.
SCOPE_NOT_BD=0

# core_rig_names prints one configured rig name per line (suspended rigs
# included: their stores still hold data). Prints nothing and returns 1 when
# the rig list cannot be read, so the caller can record that the rig scopes
# were not visited.
core_rig_names() {
    local raw
    if ! raw=$(cd "$CITY_ABS" && gc rig list --json 2>/dev/null); then
        return 1
    fi
    printf '%s\n' "$raw" | jq -r '.rigs[]? | select((.hq // false) == false) | .name // empty' 2>/dev/null
}

# scope_select <city|rig> [rig-name] makes a scope current. SCOPE_DB is left
# empty; scope_resolve_db asks bd for it.
scope_select() {
    SCOPE_KIND="$1"
    SCOPE_RIG=""
    SCOPE_DB=""
    SCOPE_LABEL="city"
    if [ "$SCOPE_KIND" = "rig" ]; then
        SCOPE_RIG="$2"
        SCOPE_LABEL="rig:$2"
    fi
}

# scope_bd runs a bd command against the current scope through gc.
scope_bd() {
    if [ -n "$SCOPE_RIG" ]; then
        (cd "$CITY_ABS" && gc bd --city "$CITY_ABS" --rig "$SCOPE_RIG" "$@")
    else
        (cd "$CITY_ABS" && gc bd --city "$CITY_ABS" "$@")
    fi
}

# scope_sql_is_read reports whether a query is a plain read (SELECT, SHOW or a
# WITH ... SELECT). Anything else is refused by scope_sql_read.
scope_sql_is_read() {
    local trimmed
    trimmed="${1#"${1%%[![:space:]]*}"}"
    case "$(printf '%.6s' "$trimmed" | tr '[:lower:]' '[:upper:]')" in
        SELECT | "SHOW "* | "WITH "* | "WITH"$'\n'* | "WITH"$'\t'*) return 0 ;;
    esac
    return 1
}

# scope_sql_read <csv|json> <query> runs a read-only query against the current
# scope. bd v1.3.0's `gc bd sql` treats `WITH [RECURSIVE] name(cols) AS (...)
# SELECT` as a write and returns no rows, so reads that start with WITH are
# wrapped in a derived table, which bd classifies as a SELECT.
scope_sql_read() {
    local format="$1"
    local query="$2"
    local trimmed

    if ! scope_sql_is_read "$query"; then
        printf 'scope_sql_read: refusing non-read query\n' >&2
        return 2
    fi
    trimmed="${query#"${query%%[![:space:]]*}"}"
    case "$(printf '%.4s' "$trimmed" | tr '[:lower:]' '[:upper:]')" in
        WITH) query="SELECT * FROM (
$query
) gc_scope_read" ;;
    esac
    scope_bd sql "--$format" "$query"
}

# scope_resolve_db asks bd which database backs the current scope and stores
# it in SCOPE_DB. The name labels archive directories and anomalies and feeds
# the workflow-root store-ref comparison; nothing dials it.
scope_resolve_db() {
    local out
    local db
    SCOPE_DB=""
    SCOPE_LAST_ERROR=""
    SCOPE_NOT_BD=0
    local err_file
    err_file=$(mktemp)
    if ! out=$(scope_sql_read csv "SELECT DATABASE()" 2>"$err_file"); then
        out="$(cat "$err_file" 2>/dev/null) $out"
        rm -f "$err_file"
        SCOPE_LAST_ERROR="$out"
        case "$out" in
            *"only supported for bd-backed beads providers"*) SCOPE_NOT_BD=1 ;;
        esac
        return 1
    fi
    rm -f "$err_file"
    db=$(printf '%s\n' "$out" | tail -n 1 | tr -d '\r')
    case "$db" in
        '' | *[!A-Za-z0-9_-]* | -*)
            SCOPE_LAST_ERROR="bd reported an unusable database name: $db"
            return 1
            ;;
    esac
    SCOPE_DB="$db"
}

# scope_bd_each_chunk <chunk-size> <ids-file> <verb> [flags...] runs
# `bd <verb> <ids...> [flags...] --json` once per chunk of ids read from
# <ids-file> (one id per line). bd's multi-id verbs (close, update) report
# per-id failures on stderr and may still exit 0, so success is counted from
# the ids bd returns in its --json array, not from the exit status.
# SCOPE_CHUNK_OK / SCOPE_CHUNK_FAILED count ids bd did / did not apply the
# verb to; SCOPE_CHUNK_FAILED_IDS and SCOPE_CHUNK_ERRORS carry the failed ids
# and bd's diagnostics. When SCOPE_DEADLINE (epoch seconds) is set and has
# passed, no further chunk is issued and the ids left over are counted in
# SCOPE_CHUNK_DEFERRED.
SCOPE_DEADLINE="${SCOPE_DEADLINE:-}"
SCOPE_CHUNK_DEFERRED=0
SCOPE_CHUNK_OK=0
SCOPE_CHUNK_FAILED=0
SCOPE_CHUNK_FAILED_IDS=""
SCOPE_CHUNK_ERRORS=""
scope_bd_each_chunk() {
    local size="$1"
    local ids_file="$2"
    local verb="$3"
    shift 3
    local chunk=()
    local id
    local out
    local err_file
    local applied
    local missing

    SCOPE_CHUNK_OK=0
    SCOPE_CHUNK_FAILED=0
    SCOPE_CHUNK_DEFERRED=0
    SCOPE_CHUNK_FAILED_IDS=""
    SCOPE_CHUNK_ERRORS=""
    case "$size" in '' | *[!0-9]* | 0) size=200 ;; esac
    err_file=$(mktemp)

    flush_chunk() {
        [ "${#chunk[@]}" -gt 0 ] || return 0
        if [ -n "$SCOPE_DEADLINE" ] && [ "$(date +%s)" -ge "$SCOPE_DEADLINE" ]; then
            SCOPE_CHUNK_DEFERRED=$((SCOPE_CHUNK_DEFERRED + ${#chunk[@]}))
            chunk=()
            return 0
        fi
        out=$(scope_bd "$verb" "${chunk[@]}" "$@" --json 2>"$err_file") || true
        applied=$(printf '%s' "$out" | jq -r 'if type == "array" then .[].id // empty elif type == "object" then .id // empty else empty end' 2>/dev/null || true)
        missing=""
        for id in "${chunk[@]}"; do
            if printf '%s\n' "$applied" | grep -qxF -- "$id"; then
                SCOPE_CHUNK_OK=$((SCOPE_CHUNK_OK + 1))
            else
                SCOPE_CHUNK_FAILED=$((SCOPE_CHUNK_FAILED + 1))
                missing="${missing:+$missing }$id"
            fi
        done
        if [ -n "$missing" ]; then
            SCOPE_CHUNK_FAILED_IDS="${SCOPE_CHUNK_FAILED_IDS:+$SCOPE_CHUNK_FAILED_IDS }$missing"
            SCOPE_CHUNK_ERRORS="${SCOPE_CHUNK_ERRORS}$(cat "$err_file" 2>/dev/null)${out:+ $out}
"
        fi
        chunk=()
    }

    while IFS= read -r id; do
        id="${id%$'\r'}"
        [ -n "$id" ] || continue
        chunk+=("$id")
        if [ "${#chunk[@]}" -ge "$size" ]; then
            flush_chunk "$@"
        fi
    done <"$ids_file"
    flush_chunk "$@"
    rm -f "$err_file"
}
