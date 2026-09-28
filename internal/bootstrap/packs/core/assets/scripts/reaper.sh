#!/usr/bin/env bash
# reaper — close stale wisps with closed parents/roots, purge old closed wisps,
# auto-close stale and TTL-expired issues, and prune closed session beads.
#
# Core exec order. Every bead scope (the city and each rig) is visited through
# `gc bd --city <city> [--rig <rig>]`, and bd picks the transport for it, so
# this script is the same for proxied, direct-server and mixed cities.
# Selections that no gc bd verb can express run as read-only `gc bd sql` queries;
# every mutation is a gc bd verb (close, update, purge, prune, delete) that owns
# its own commit.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "reaper"

CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CITY_ABS="$(cd "$CITY" 2>/dev/null && pwd -P || printf '%s\n' "$CITY")"
CITY_BEADS_DIR="$CITY_ABS/.beads"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/scope_bd.sh"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/order_outcome.sh"

if ! command -v jq >/dev/null 2>&1; then
    echo "reaper: jq is required but not found in PATH" >&2
    exit 1
fi

resolve_escalate_script() {
    local candidate
    local pack
    local system_packs="${GC_SYSTEM_PACKS_DIR:-$CITY/.gc/system/packs}"

    if [ -n "${GC_ESCALATE_SCRIPT:-}" ]; then
        printf '%s\n' "$GC_ESCALATE_SCRIPT"
        return
    fi
    for pack in ${GC_ESCALATE_SEARCH_PACKS:-gastown maintenance bd core}; do
        candidate="$system_packs/$pack/assets/scripts/escalate.sh"
        if [ -x "$candidate" ]; then
            printf '%s\n' "$candidate"
            return
        fi
    done
    printf '%s\n' "$SCRIPT_DIR/escalate.sh"
}

ESCALATE_SCRIPT="$(resolve_escalate_script)"

maintenance_done() {
    local summary="$1"
    local target="${GC_MAINTENANCE_DONE_TARGET:-}"

    [ -n "$target" ] || return 0
    gc session nudge "$target" "MAINTENANCE_DONE: $summary" 2>/dev/null || true
}

# Configurable thresholds.
MAX_AGE="${GC_REAPER_MAX_AGE:-24h}"
PURGE_AGE="${GC_REAPER_PURGE_AGE:-168h}"
STALE_ISSUE_AGE="${GC_REAPER_STALE_ISSUE_AGE:-720h}"
SESSION_PURGE_AGE="${GC_REAPER_SESSION_PURGE_AGE:-720h}"
SESSION_BEAD_PATTERN="${GC_REAPER_SESSION_BEAD_PATTERN-gm-*}"
SESSION_STATE_PRUNE_AGE="${GC_REAPER_SESSION_STATE_PRUNE_AGE:-24h}"
ALERT_THRESHOLD="${GC_REAPER_ALERT_THRESHOLD:-500}"
MAIL_ALERT_THRESHOLD="${GC_REAPER_MAIL_ALERT_THRESHOLD:-0}"  # 0 = disabled
DRY_RUN="${GC_REAPER_DRY_RUN:-}"
# Ids handed to one gc bd verb invocation (close/update/delete).
VERB_BATCH="${GC_REAPER_VERB_BATCH:-200}"
case "$VERB_BATCH" in ''|*[!0-9]*|0) VERB_BATCH=200 ;; esac
# Closing follows only ownership edges. `blocks` is sequencing, not ownership.
WISP_CLOSE_EDGE_PREDICATE="(d.type = 'parent-child' OR (d.type = 'tracks' AND JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.root_bead_id\"')) = COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)))"
WORKFLOW_ROOT_CLOSE_STATUSES="'open', 'hooked', 'in_progress'"
WORKFLOW_ROOT_LIVE_STATUSES="'open', 'hooked', 'in_progress', 'blocked', 'deferred', 'pinned', 'review', 'testing'"
WORKFLOW_ROOT_DESCENDANT_DEP_TYPES="'parent-child', 'tracks', 'blocks'"
WORKFLOW_ROOT_CLOSE_REASON="stale inactive workflow root auto-closed by reaper"
STALE_WISP_CLOSE_REASON="stale wisp whose owning parent or root is closed, auto-closed by reaper"
# Step 3 purges the whole wisps plane (including --no-history rows, which is
# where gc keeps order-tracking beads) and relies on gc bd purge to keep a closed
# wisp that an open, hooked or in-progress wisp still depends on through
# parent-child, tracks or blocks.
PURGE_PLANE_FLAG="${GC_REAPER_PURGE_PLANE_FLAG:---wisps-plane}"
# Closed wisps deleted per bd purge call, and the wall-clock budget (seconds,
# shared by every scope) the purge step may spend per run. Both keep a large
# backlog from outrunning the order timeout.
PURGE_BATCH="${GC_REAPER_PURGE_BATCH:-500}"
case "$PURGE_BATCH" in ''|*[!0-9]*|0) PURGE_BATCH=500 ;; esac
PURGE_BUDGET_SECS="${GC_REAPER_PURGE_BUDGET_SECS:-300}"
case "$PURGE_BUDGET_SECS" in ''|*[!0-9]*) PURGE_BUDGET_SECS=300 ;; esac
# The purge budget starts with the first purge call (Step 3), not with the run.
PURGE_DEADLINE=""
# Wall-clock budget (seconds) for the whole run, below the order's 900s
# timeout. Once it is spent the run stops starting new work, declares a
# partial outcome, and the next run picks up where this one stopped (every
# selection is recomputed from the store). A killed run would instead record
# order.failed with no outcome and starve the scopes after it.
RUN_BUDGET_SECS="${GC_REAPER_RUN_BUDGET_SECS:-780}"
case "$RUN_BUDGET_SECS" in ''|*[!0-9]*) RUN_BUDGET_SECS=780 ;; esac
RUN_DEADLINE=$(( $(date +%s) + RUN_BUDGET_SECS ))
# scope_bd_each_chunk (scope_bd.sh) stops issuing verb chunks at this deadline.
# shellcheck disable=SC2034
SCOPE_DEADLINE="$RUN_DEADLINE"
RUN_BUDGET_HIT=0

# Convert Go-style hour durations to SQL INTERVAL hours.
duration_to_hours() {
    local dur="$1"
    echo "${dur%h}"
}

MAX_AGE_H=$(duration_to_hours "$MAX_AGE")
STALE_AGE_H=$(duration_to_hours "$STALE_ISSUE_AGE")

TOTAL_SCOPES=0
TOTAL_STALE_WISPS=0
TOTAL_CLOSED_WISPS=0
TOTAL_WOULD_CLOSE_WISPS=0
TOTAL_HELD_WISPS=0
TOTAL_WOULD_EXPIRE=0
TOTAL_PURGED=0
TOTAL_WOULD_PURGE=0
TOTAL_MAIL_WISPS=0
TOTAL_WORKFLOW_ROOTS_CLOSED=0
TOTAL_WOULD_CLOSE_WORKFLOW_ROOTS=0
TOTAL_WORKFLOW_ROOTS_STORE_REF_SKIPPED=0
TOTAL_WORKFLOW_ISSUE_ROOTS_SKIPPED=0
TOTAL_ISSUES_CLOSED=0
TOTAL_STALE_ISSUES_SKIPPED=0
TOTAL_EXPIRED_ISSUES_CLOSED=0
TOTAL_EXPIRED_ISSUES_SKIPPED=0
TOTAL_SESSIONS_PRUNED=0
ANOMALIES=""
CITY_DB=""

# run_budget_exhausted reports whether the run budget is spent, declaring the
# partial outcome the first time.
run_budget_exhausted() {
    if [ "$RUN_BUDGET_HIT" -eq 0 ] && [ "$(date +%s)" -lt "$RUN_DEADLINE" ]; then
        return 1
    fi
    if [ "$RUN_BUDGET_HIT" -eq 0 ]; then
        RUN_BUDGET_HIT=1
        order_outcome_scope_skipped "${SCOPE_LABEL:-city}" "run budget exhausted; remaining work carries to the next run"
    fi
    return 0
}

sanitize_output() {
    local flattened
    flattened=$(printf '%s' "$1" | tr '\n' ' ')
    if [ "${#flattened}" -le 4000 ]; then
        printf '%s' "$flattened"
    else
        printf '%s...[truncated]...%s' "${flattened:0:2000}" "${flattened: -1900}"
    fi
}

record_anomaly() {
    local scope="$1"
    shift
    ANOMALIES="${ANOMALIES}$scope: $*
"
}

# scope_anomaly records an anomaly keyed by the current scope's database (its
# label when the database is not known yet).
scope_anomaly() {
    record_anomaly "${SCOPE_DB:-$SCOPE_LABEL}" "$@"
}

# workflow_root_store_ref_local_condition keeps workflow roots whose
# gc.root_store_ref names this scope (or is unset). Roots stamped for another
# store are skipped: their subtrees need cross-store traversal first.
workflow_root_store_ref_local_condition() {
    local db="$1"
    local alias="$2"

    cat <<SQL
                COALESCE(JSON_UNQUOTE(JSON_EXTRACT($alias.metadata, '$."gc.root_store_ref"')), '') = ''
                OR JSON_UNQUOTE(JSON_EXTRACT($alias.metadata, '$."gc.root_store_ref"')) = '$db'
SQL
    if [ "$SCOPE_KIND" = "city" ]; then
        cat <<SQL
                OR JSON_UNQUOTE(JSON_EXTRACT($alias.metadata, '$."gc.root_store_ref"')) LIKE 'city:%'
SQL
    fi
    if [ -n "$SCOPE_RIG_REFS" ]; then
        cat <<SQL
                OR JSON_UNQUOTE(JSON_EXTRACT($alias.metadata, '$."gc.root_store_ref"')) IN ($SCOPE_RIG_REFS)
SQL
    fi
}

SQL_COUNT_RESULT=0
get_sql_count() {
    local label="$1"
    local query="$2"
    local output
    local stderr_file
    local stderr_output
    local count

    SQL_COUNT_RESULT=0
    if ! stderr_file=$(mktemp); then
        scope_anomaly "$label count failed: could not create stderr capture file"
        return 1
    fi
    if ! output=$(scope_sql_read csv "$query" 2>"$stderr_file"); then
        stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
        rm -f "$stderr_file"
        scope_anomaly "$label count failed for $SCOPE_DB: $(sanitize_output "$stderr_output $output")"
        return 1
    fi
    rm -f "$stderr_file"

    count=$(printf '%s\n' "$output" | tail -1 | tr -d '\r')
    if [ -z "$count" ] || ! [[ "$count" =~ ^[0-9]+$ ]]; then
        scope_anomaly "$label count returned non-numeric value for $SCOPE_DB: $(sanitize_output "$output")"
        return 1
    fi

    SQL_COUNT_RESULT="$count"
}

SQL_ROWS_RESULT=""
get_sql_rows() {
    local label="$1"
    local query="$2"
    local output
    local stderr_file
    local stderr_output

    SQL_ROWS_RESULT=""
    if ! stderr_file=$(mktemp); then
        scope_anomaly "$label query failed: could not create stderr capture file"
        return 1
    fi
    if ! output=$(scope_sql_read csv "$query" 2>"$stderr_file"); then
        stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
        rm -f "$stderr_file"
        scope_anomaly "$label query failed for $SCOPE_DB: $(sanitize_output "$stderr_output $output")"
        return 1
    fi
    rm -f "$stderr_file"

    SQL_ROWS_RESULT=$(printf '%s\n' "$output" | tail -n +2 | tr -d '\r' | sed '/^[[:space:]]*$/d')
}

count_lines() {
    if [ -z "$1" ]; then
        printf '0\n'
        return
    fi
    printf '%s\n' "$1" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' '
}

# close_ids <label> <ids> <reason> [force] closes ids in the current scope
# through `gc bd close`, in chunks. Pass a non-empty fourth argument for --force:
# bd's close-authority guard (gastownhall/beads#3734) refuses a cross-actor
# close without it, so rows another actor still holds need it, while rows
# that were unassigned at selection stay bare so the guard still rejects a
# close that would clobber a concurrent re-claim. CLOSE_IDS_OK holds the
# number of ids in chunks bd accepted.
CLOSE_IDS_OK=0
CLOSE_IDS_FAILED=""
close_ids() {
    local label="$1"
    local ids="$2"
    local reason="$3"
    local force="${4:-}"
    local ids_file

    CLOSE_IDS_OK=0
    SCOPE_CHUNK_FAILED_IDS=""
    [ -n "$ids" ] || return 0
    ids_file=$(mktemp)
    printf '%s\n' "$ids" >"$ids_file"
    if [ -n "$force" ]; then
        scope_bd_each_chunk "$VERB_BATCH" "$ids_file" close --force --reason "$reason"
    else
        scope_bd_each_chunk "$VERB_BATCH" "$ids_file" close --reason "$reason"
    fi
    rm -f "$ids_file"
    CLOSE_IDS_OK=$SCOPE_CHUNK_OK
    if [ "$SCOPE_CHUNK_DEFERRED" -gt 0 ]; then
        run_budget_exhausted || true
    fi
    if [ "$SCOPE_CHUNK_FAILED" -gt 0 ]; then
        scope_anomaly "$label: gc bd close failed for $SCOPE_CHUNK_FAILED bead(s) in $SCOPE_DB ($(sanitize_output "$SCOPE_CHUNK_FAILED_IDS")): $(sanitize_output "$SCOPE_CHUNK_ERRORS")"
    fi
}

# close_ids_by_mode closes "<id>,<bare|force>" rows: bare rows without
# --force, force rows with it (see close_ids).
close_ids_by_mode() {
    local label="$1"
    local rows="$2"
    local reason="$3"
    local bare_ids
    local force_ids
    local total=0

    local failed=""

    bare_ids=$(printf '%s\n' "$rows" | awk -F, '$1 != "" && $2 != "force" { print $1 }')
    force_ids=$(printf '%s\n' "$rows" | awk -F, '$1 != "" && $2 == "force" { print $1 }')
    close_ids "$label" "$bare_ids" "$reason"
    total=$CLOSE_IDS_OK
    failed="$SCOPE_CHUNK_FAILED_IDS"
    close_ids "$label" "$force_ids" "$reason" force
    CLOSE_IDS_OK=$((total + CLOSE_IDS_OK))
    CLOSE_IDS_FAILED="${failed}${failed:+ }${SCOPE_CHUNK_FAILED_IDS}"
}

workflow_root_candidates_cte() {
    local db="$1"
    local candidate_cte="$2"
    local table="$3"
    local alias="$4"
    local issue_type_exclusions="$5"

    cat <<SQL
        WITH RECURSIVE ${candidate_cte}_base(id) AS (
            SELECT $alias.id FROM \`$db\`.$table $alias
            WHERE $alias.status IN ($WORKFLOW_ROOT_CLOSE_STATUSES)
            AND $alias.issue_type NOT IN ($issue_type_exclusions)
            AND COALESCE($alias.assignee, '') = ''
            AND $alias.created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            AND COALESCE($alias.updated_at, $alias.created_at) < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            AND (
                JSON_UNQUOTE(JSON_EXTRACT($alias.metadata, '$."gc.kind"')) = 'workflow'
                OR JSON_UNQUOTE(JSON_EXTRACT($alias.metadata, '$."gc.formula_contract"')) = 'graph.v2'
            )
            AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT($alias.metadata, '$."gc.root_bead_id"')), '') IN ('', $alias.id)
        ),
        $candidate_cte(id) AS (
            SELECT base.id
            FROM ${candidate_cte}_base base
            INNER JOIN \`$db\`.$table $alias ON $alias.id = base.id
            WHERE (
$(workflow_root_store_ref_local_condition "$db" "$alias")
            )
        ),
        workflow_descendants(root_id, id) AS (
            SELECT root.id, child_wisp.id
            FROM $candidate_cte root
            INNER JOIN \`$db\`.wisps child_wisp
                ON child_wisp.id != root.id
                AND JSON_UNQUOTE(JSON_EXTRACT(child_wisp.metadata, '$."gc.root_bead_id"')) = root.id
            UNION
            SELECT root.id, child_issue.id
            FROM $candidate_cte root
            INNER JOIN \`$db\`.issues child_issue
                ON child_issue.id != root.id
                AND JSON_UNQUOTE(JSON_EXTRACT(child_issue.metadata, '$."gc.root_bead_id"')) = root.id
            UNION
            SELECT root.id, child_dep.issue_id
            FROM $candidate_cte root
            INNER JOIN \`$db\`.wisp_dependencies child_dep
                ON child_dep.type IN ($WORKFLOW_ROOT_DESCENDANT_DEP_TYPES)
                AND COALESCE(child_dep.depends_on_issue_id, child_dep.depends_on_wisp_id, child_dep.depends_on_external) = root.id
                AND child_dep.issue_id != root.id
            UNION
            SELECT root.id, child_dep.issue_id
            FROM $candidate_cte root
            INNER JOIN \`$db\`.dependencies child_dep
                ON child_dep.type IN ($WORKFLOW_ROOT_DESCENDANT_DEP_TYPES)
                AND COALESCE(child_dep.depends_on_issue_id, child_dep.depends_on_wisp_id, child_dep.depends_on_external) = root.id
                AND child_dep.issue_id != root.id
            UNION
            SELECT parent.root_id, child_dep.issue_id
            FROM workflow_descendants parent
            INNER JOIN \`$db\`.wisp_dependencies child_dep
                ON child_dep.type IN ($WORKFLOW_ROOT_DESCENDANT_DEP_TYPES)
                AND COALESCE(child_dep.depends_on_issue_id, child_dep.depends_on_wisp_id, child_dep.depends_on_external) = parent.id
                AND child_dep.issue_id != parent.id
            UNION
            SELECT parent.root_id, child_dep.issue_id
            FROM workflow_descendants parent
            INNER JOIN \`$db\`.dependencies child_dep
                ON child_dep.type IN ($WORKFLOW_ROOT_DESCENDANT_DEP_TYPES)
                AND COALESCE(child_dep.depends_on_issue_id, child_dep.depends_on_wisp_id, child_dep.depends_on_external) = parent.id
                AND child_dep.issue_id != parent.id
        ),
        roots_with_live_descendants AS (
            SELECT DISTINCT descendant.root_id
            FROM workflow_descendants descendant
            LEFT JOIN \`$db\`.wisps descendant_wisp ON descendant_wisp.id = descendant.id
            LEFT JOIN \`$db\`.issues descendant_issue ON descendant_issue.id = descendant.id
            WHERE COALESCE(descendant_wisp.status, descendant_issue.status) IN ($WORKFLOW_ROOT_LIVE_STATUSES)
        ),
        roots_with_recent_descendants AS (
            SELECT DISTINCT descendant.root_id
            FROM workflow_descendants descendant
            LEFT JOIN \`$db\`.wisps descendant_wisp ON descendant_wisp.id = descendant.id
            LEFT JOIN \`$db\`.issues descendant_issue ON descendant_issue.id = descendant.id
            WHERE COALESCE(
                descendant_wisp.updated_at,
                descendant_wisp.created_at,
                descendant_issue.updated_at,
                descendant_issue.created_at
            ) >= DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
        )
SQL
}

workflow_root_store_ref_skipped_count_query() {
    local db="$1"
    local candidate_cte="$2"
    local table="$3"
    local alias="$4"
    local issue_type_exclusions="$5"

    cat <<SQL
$(workflow_root_candidates_cte "$db" "$candidate_cte" "$table" "$alias" "$issue_type_exclusions")
        SELECT COUNT(*) FROM ${candidate_cte}_base base
        LEFT JOIN $candidate_cte candidate ON candidate.id = base.id
        WHERE candidate.id IS NULL
SQL
}

workflow_root_closeable_select() {
    local candidate_cte="$1"

    cat <<SQL
        SELECT DISTINCT root.id
        FROM $candidate_cte root
        LEFT JOIN roots_with_live_descendants live ON live.root_id = root.id
        LEFT JOIN roots_with_recent_descendants recent ON recent.root_id = root.id
        WHERE live.root_id IS NULL
        AND recent.root_id IS NULL
SQL
}

workflow_root_count_query() {
    local db="$1"
    local candidate_cte="$2"
    local table="$3"
    local alias="$4"
    local issue_type_exclusions="$5"

    cat <<SQL
$(workflow_root_candidates_cte "$db" "$candidate_cte" "$table" "$alias" "$issue_type_exclusions")
        SELECT COUNT(*) FROM (
$(workflow_root_closeable_select "$candidate_cte")
        ) closeable_workflow_roots
SQL
}

workflow_root_ids_query() {
    local db="$1"
    local candidate_cte="$2"
    local table="$3"
    local alias="$4"
    local issue_type_exclusions="$5"

    cat <<SQL
$(workflow_root_candidates_cte "$db" "$candidate_cte" "$table" "$alias" "$issue_type_exclusions")
$(workflow_root_closeable_select "$candidate_cte")
SQL
}

# stale_wisp_subtree_query selects, for Step 1, every stale open wisp whose
# ownership edge points to a closed parent/root (the subtree roots) and every
# non-closed descendant reachable from them through ownership edges (wisps)
# or parent-child edges (durable issues). One row per (node, parent) edge:
# id, owner_id, depth, ok|keep, bare|force. "ok" marks a wisp the reaper may
# close (open/hooked/in_progress and older than MAX_AGE); anything else is
# "keep".
stale_wisp_subtree_query() {
    local db="$1"

    cat <<SQL
        WITH RECURSIVE reap_roots(id) AS (
            SELECT DISTINCT w.id
            FROM \`$db\`.wisps w
            INNER JOIN \`$db\`.wisp_dependencies d
                ON d.issue_id = w.id
                AND $WISP_CLOSE_EDGE_PREDICATE
            LEFT JOIN \`$db\`.wisps parent_wisp ON d.depends_on_wisp_id = parent_wisp.id
            LEFT JOIN \`$db\`.issues parent_issue ON d.depends_on_issue_id = parent_issue.id
            WHERE w.status IN ('open', 'hooked', 'in_progress')
            AND w.created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            AND (
                parent_wisp.status = 'closed'
                OR parent_issue.status = 'closed'
            )
        ),
        reap_edges(child_id, owner_id) AS (
            SELECT d.issue_id, COALESCE(d.depends_on_wisp_id, d.depends_on_issue_id, d.depends_on_external)
            FROM \`$db\`.wisp_dependencies d
            INNER JOIN \`$db\`.wisps w ON w.id = d.issue_id
            WHERE w.status != 'closed'
            AND $WISP_CLOSE_EDGE_PREDICATE
            UNION ALL
            SELECT d.issue_id, COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)
            FROM \`$db\`.dependencies d
            INNER JOIN \`$db\`.issues i ON i.id = d.issue_id
            WHERE i.status != 'closed'
            AND d.type = 'parent-child'
        ),
        reap_tree(id, owner_id, depth) AS (
            SELECT id, CAST('' AS CHAR(255)), 0 FROM reap_roots
            UNION ALL
            SELECT e.child_id, t.id, t.depth + 1
            FROM reap_tree t
            INNER JOIN reap_edges e ON e.owner_id = t.id
            WHERE t.depth < 32
        )
        SELECT t.id, t.owner_id, t.depth,
            CASE WHEN w.id IS NOT NULL
                AND w.status IN ('open', 'hooked', 'in_progress')
                AND w.created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            THEN 'ok' ELSE 'keep' END,
            CASE WHEN COALESCE(w.assignee, '') = '' THEN 'bare' ELSE 'force' END
        FROM reap_tree t
        LEFT JOIN \`$db\`.wisps w ON w.id = t.id
SQL
}

# closeable_subtree_leaf_first reads stale_wisp_subtree_query rows on stdin
# and prints "depth,id,bare|force" for every wisp that may close: a node is
# held open when it is not closeable itself or when any descendant is held
# open, so a printed node's open children are all printed too, deeper. The
# caller closes deepest first.
#
# With "held" as $1 it prints instead the ids of the stale wisps held open by
# a descendant the reaper may not close (their own state is "ok").
closeable_subtree_leaf_first() {
    awk -F, -v want="${1:-closeable}" '
        NF >= 5 && $1 != "" {
            id = $1
            if (!(id in depth) || $3 + 0 > depth[id]) depth[id] = $3 + 0
            if ($4 != "ok") { held[id] = 1; notok[id] = 1 }
            mode[id] = $5
            if ($2 != "") parents[id] = parents[id] " " $2
        }
        END {
            changed = 1
            while (changed) {
                changed = 0
                for (id in held) {
                    n = split(parents[id], ps, " ")
                    for (i = 1; i <= n; i++) {
                        if (ps[i] != "" && !(ps[i] in held)) {
                            held[ps[i]] = 1
                            changed = 1
                        }
                    }
                }
            }
            for (id in depth) {
                if (want == "held") {
                    if ((id in held) && !(id in notok)) print id
                } else if (!(id in held)) {
                    print depth[id] "," id "," mode[id]
                }
            }
        }'
}

# subtree_ancestors <subtree-rows> <ids...> prints every owner, transitively,
# of the given ids in stale_wisp_subtree_query rows.
subtree_ancestors() {
    local tree="$1"
    shift
    printf '%s\n' "$tree" | awk -F, -v seeds="$*" '
        NF >= 5 && $2 != "" { parents[$1] = parents[$1] " " $2 }
        END {
            n = split(seeds, queue, " ")
            for (i = 1; i <= n; i++) seen[queue[i]] = 1
            head = 1
            while (head <= n) {
                id = queue[head++]
                m = split(parents[id], ps, " ")
                for (j = 1; j <= m; j++) {
                    p = ps[j]
                    if (p != "" && !(p in seen)) {
                        seen[p] = 1
                        queue[++n] = p
                        print p
                    }
                }
            }
        }'
}

# reap_scope runs Steps 1-6b against the current scope.
reap_scope() {
    local DB="$SCOPE_DB"
    local stale_wisp_count
    local rows
    local ids
    local count

    # Step 1: close stale non-closed wisps whose ownership edge points to a
    # closed parent/root, together with their stale ownership descendants.
    # Wisps without an ownership edge are counted but not closed by age alone.
    #
    # bd refuses to close a bead that still has open children, and a wisp
    # under a closed owner can itself own open wisps (root closed -> mid open
    # -> leaf open). So the whole orphaned subtree is selected at once and
    # closed deepest-first: every child is closed before its parent. A wisp
    # with any open descendant that is not itself closeable (too young, not
    # a wisp, or in a status the reaper does not close) keeps its whole
    # ancestry open, so no close — bare or --force — ever lands on a bead
    # whose open child stays open.
    get_sql_count "stale non-closed wisp" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
        AND issue_type NOT IN ('message')
        AND created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
    " || true
    stale_wisp_count=$SQL_COUNT_RESULT
    TOTAL_STALE_WISPS=$((TOTAL_STALE_WISPS + stale_wisp_count))

    if [ "$stale_wisp_count" -gt 0 ] && get_sql_rows "stale wisp subtree" "$(stale_wisp_subtree_query "$DB")"; then
        local tree=$SQL_ROWS_RESULT
        local held_count
        rows=$(printf '%s\n' "$tree" | closeable_subtree_leaf_first)
        count=$(count_lines "$rows")
        # Stale wisps held open by a descendant the reaper may not close (a
        # durable issue, a blocked or young wisp) are reported every run
        # rather than closed; they clear once that descendant closes.
        held_count=$(count_lines "$(printf '%s\n' "$tree" | closeable_subtree_leaf_first held)")
        if [ "$held_count" -gt 0 ]; then
            TOTAL_HELD_WISPS=$((TOTAL_HELD_WISPS + held_count))
            order_outcome_scope_skipped "$SCOPE_LABEL" "$held_count stale wisp(s) held open under a live descendant"
        fi
        if [ "$count" -gt 0 ]; then
            if [ -n "$DRY_RUN" ]; then
                TOTAL_WOULD_CLOSE_WISPS=$((TOTAL_WOULD_CLOSE_WISPS + count))
            else
                local depth
                local dropped
                local level
                for depth in $(printf '%s\n' "$rows" | awk -F, '{ print $1 }' | sort -rnu); do
                    run_budget_exhausted && break
                    level=$(printf '%s\n' "$rows" | awk -F, -v d="$depth" '$1 == d { print $2 "," $3 }')
                    [ -n "$level" ] || continue
                    close_ids_by_mode "closing stale wisps" "$level" "$STALE_WISP_CLOSE_REASON"
                    TOTAL_CLOSED_WISPS=$((TOTAL_CLOSED_WISPS + CLOSE_IDS_OK))
                    if [ -n "$CLOSE_IDS_FAILED" ]; then
                        # A close bd rejected at run time (e.g. a leaf
                        # re-claimed since selection) leaves that bead open,
                        # so none of its owners may close this run -- not
                        # even with --force.
                        # shellcheck disable=SC2086 # space-separated ids
                        dropped=$(subtree_ancestors "$tree" $CLOSE_IDS_FAILED)
                        if [ -n "$dropped" ]; then
                            rows=$(printf '%s\n' "$rows" | awk -F, -v drop="$(printf '%s' "$dropped" | tr '\n' ' ')" '
                                BEGIN { n = split(drop, d, " "); for (i = 1; i <= n; i++) skip[d[i]] = 1 }
                                !($2 in skip)')
                        fi
                    fi
                done
            fi
        fi
    fi

    run_budget_exhausted && return 0

    # Step 2: close stale inactive workflow roots. This is the finalize-crash
    # safety net: it only reaps old, unassigned topology roots whose stamped
    # and parent-child subtree has no live descendants. Roots stamped with
    # gc.root_store_ref for another store are skipped; cross-store subtrees
    # need cross-store traversal before reaping can be safe. Wisp roots are
    # closed in every scope; issue roots are city issues and close only in the
    # city scope.
    if get_sql_count "workflow wisp roots skipped by root store ref" "$(workflow_root_store_ref_skipped_count_query "$DB" "workflow_wisp_root_candidates" "wisps" "w" "'message'")"; then
        TOTAL_WORKFLOW_ROOTS_STORE_REF_SKIPPED=$((TOTAL_WORKFLOW_ROOTS_STORE_REF_SKIPPED + SQL_COUNT_RESULT))
    fi

    if get_sql_rows "stale inactive workflow wisp root" "$(workflow_root_ids_query "$DB" "workflow_wisp_root_candidates" "wisps" "w" "'message'")"; then
        ids=$SQL_ROWS_RESULT
        count=$(count_lines "$ids")
        if [ "$count" -gt 0 ]; then
            if [ -n "$DRY_RUN" ]; then
                TOTAL_WOULD_CLOSE_WORKFLOW_ROOTS=$((TOTAL_WOULD_CLOSE_WORKFLOW_ROOTS + count))
            else
                # Stamp the skipped outcome first: a crash between the two
                # verbs leaves an open root that the next run selects again.
                local ids_file
                ids_file=$(mktemp)
                printf '%s\n' "$ids" >"$ids_file"
                scope_bd_each_chunk "$VERB_BATCH" "$ids_file" update \
                    --set-metadata "gc.outcome=skipped" \
                    --set-metadata "close_reason=$WORKFLOW_ROOT_CLOSE_REASON"
                rm -f "$ids_file"
                if [ "$SCOPE_CHUNK_FAILED" -gt 0 ]; then
                    scope_anomaly "stamping stale workflow wisp roots failed for $SCOPE_CHUNK_FAILED bead(s) in $DB: $(sanitize_output "$SCOPE_CHUNK_ERRORS")"
                fi
                if [ "$SCOPE_CHUNK_OK" -gt 0 ]; then
                    close_ids "closing stale inactive workflow wisp roots" "$ids" "$WORKFLOW_ROOT_CLOSE_REASON"
                    TOTAL_WORKFLOW_ROOTS_CLOSED=$((TOTAL_WORKFLOW_ROOTS_CLOSED + CLOSE_IDS_OK))
                fi
            fi
        fi
    fi

    if get_sql_count "workflow issue roots skipped by root store ref" "$(workflow_root_store_ref_skipped_count_query "$DB" "workflow_issue_root_candidates" "issues" "i" "'message', 'epic'")"; then
        TOTAL_WORKFLOW_ROOTS_STORE_REF_SKIPPED=$((TOTAL_WORKFLOW_ROOTS_STORE_REF_SKIPPED + SQL_COUNT_RESULT))
    fi

    if get_sql_rows "stale inactive workflow issue root" "$(workflow_root_ids_query "$DB" "workflow_issue_root_candidates" "issues" "i" "'message', 'epic'")"; then
        ids=$SQL_ROWS_RESULT
        count=$(count_lines "$ids")
        if [ "$count" -gt 0 ]; then
            if [ "$SCOPE_KIND" != "city" ]; then
                TOTAL_WORKFLOW_ISSUE_ROOTS_SKIPPED=$((TOTAL_WORKFLOW_ISSUE_ROOTS_SKIPPED + count))
            elif [ -n "$DRY_RUN" ]; then
                TOTAL_WOULD_CLOSE_WORKFLOW_ROOTS=$((TOTAL_WOULD_CLOSE_WORKFLOW_ROOTS + count))
            else
                close_ids "closing stale inactive workflow issue roots" "$ids" "$WORKFLOW_ROOT_CLOSE_REASON"
                TOTAL_WORKFLOW_ROOTS_CLOSED=$((TOTAL_WORKFLOW_ROOTS_CLOSED + CLOSE_IDS_OK))
            fi
        fi
    fi

    run_budget_exhausted && return 0

    # Step 3: purge closed wisps past purge_age through gc bd purge, which keeps
    # a closed wisp that a live wisp still depends on (parent-child, tracks,
    # blocks) and removes each purged wisp's labels, dependencies, comments
    # and events with it.
    reap_scope_purge

    run_budget_exhausted && return 0

    # Step 4: close gc:nudge beads whose metadata.expires_at is in the past.
    # Only beads labelled gc:nudge are candidates — other bead types that stamp
    # expires_at (e.g. gc:extmsg-binding session bindings) must not be closed
    # here. The COALESCE handles whole-second RFC3339+Z, microsecond-width
    # RFC3339 (MySQL %f tops out at 6 fractional digits), and full
    # RFC3339Nano (7-9 fractional digits) by truncating the fractional part to
    # whole seconds for parsing. Rows where every pattern fails STR_TO_DATE
    # return NULL and are recorded as anomalies rather than silently skipped.
    # Nudge shadows land in the sling target's store (a rig for a rig-scoped
    # sling, gastownhall/gascity#5285), so this runs in every scope.
    if get_sql_rows "expired nudge bead with parse anomaly" "
        SELECT i.id
        FROM \`$DB\`.issues i
        INNER JOIN \`$DB\`.labels lbl ON lbl.issue_id = i.id AND lbl.label = 'gc:nudge'
        WHERE i.status IN ('open', 'in_progress')
        AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')) IS NOT NULL
        AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')) != ''
        AND COALESCE(
            STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '%Y-%m-%dT%H:%i:%s.%fZ'),
            STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '%Y-%m-%dT%H:%i:%sZ'),
            STR_TO_DATE(CONCAT(SUBSTRING_INDEX(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '.', 1), 'Z'), '%Y-%m-%dT%H:%i:%sZ')
        ) IS NULL
    "; then
        while IFS= read -r bad_id; do
            [ -z "$bad_id" ] && continue
            scope_anomaly "nudge bead $bad_id in $DB has unparseable expires_at; skipped by TTL reaper"
        done <<< "$SQL_ROWS_RESULT"
    fi

    # Expired nudge beads are closed bare (no --force), which is only safe
    # when they are unassigned: bd's cross-actor close guard would otherwise
    # reject the reaper's bare close and the expired nudge would leak. Nudge
    # shadow beads are created unassigned, so the COALESCE(i.assignee,'')=''
    # filter makes that invariant explicit. An assigned expired nudge is a
    # producer anomaly and is skipped here and by the stale path below.
    if get_sql_rows "expired nudge bead" "
        SELECT i.id
        FROM \`$DB\`.issues i
        INNER JOIN \`$DB\`.labels lbl ON lbl.issue_id = i.id AND lbl.label = 'gc:nudge'
        WHERE i.status IN ('open', 'in_progress')
        AND COALESCE(i.assignee, '') = ''
        AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')) IS NOT NULL
        AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')) != ''
        AND COALESCE(
            STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '%Y-%m-%dT%H:%i:%s.%fZ'),
            STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '%Y-%m-%dT%H:%i:%sZ'),
            STR_TO_DATE(CONCAT(SUBSTRING_INDEX(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '.', 1), 'Z'), '%Y-%m-%dT%H:%i:%sZ')
        ) < UTC_TIMESTAMP()
    "; then
        ids=$SQL_ROWS_RESULT
        count=$(count_lines "$ids")
        TOTAL_WOULD_EXPIRE=$((TOTAL_WOULD_EXPIRE + count))
        if [ "$count" -gt 0 ] && [ -z "$DRY_RUN" ]; then
            close_ids "closing expired nudge beads" "$ids" "ttl:expired by reaper"
            TOTAL_EXPIRED_ISSUES_CLOSED=$((TOTAL_EXPIRED_ISSUES_CLOSED + CLOSE_IDS_OK))
            TOTAL_EXPIRED_ISSUES_SKIPPED=$((TOTAL_EXPIRED_ISSUES_SKIPPED + count - CLOSE_IDS_OK))
        fi
    fi

    run_budget_exhausted && return 0

    # Step 5: auto-close stale issues (exclude P0/P1, epics, durable extmsg
    # records, TTL-stamped beads and beads with an active dependency in either
    # direction). Only the city scope's issues are auto-closed; stale rig
    # issues are counted as skipped.
    if get_sql_rows "stale issue" "
        SELECT id, CASE WHEN COALESCE(assignee, '') = '' THEN 'bare' ELSE 'force' END
        FROM \`$DB\`.issues
        WHERE status IN ('open', 'in_progress')
        AND updated_at < DATE_SUB(NOW(), INTERVAL $STALE_AGE_H HOUR)
        AND priority > 1
        AND issue_type != 'epic'
        AND (
            JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at')) IS NULL
            OR JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at')) = ''
        )
        AND NOT EXISTS (
            SELECT 1 FROM \`$DB\`.labels lbl
            WHERE lbl.issue_id = \`$DB\`.issues.id
            AND lbl.label IN (
                'gc:extmsg-group',
                'gc:extmsg-participant',
                'gc:extmsg-binding',
                'gc:extmsg-membership',
                'gc:extmsg-transcript-state',
                'gc:extmsg-transcript'
            )
        )
        AND id NOT IN (
            SELECT DISTINCT d.issue_id FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.issues i ON d.depends_on_issue_id = i.id
            WHERE i.status IN ('open', 'in_progress')
            AND d.issue_id IS NOT NULL
            UNION
            SELECT DISTINCT d.depends_on_issue_id FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.issues i ON d.issue_id = i.id
            WHERE i.status IN ('open', 'in_progress')
            AND d.depends_on_issue_id IS NOT NULL
        )
    "; then
        rows=$SQL_ROWS_RESULT
        count=$(count_lines "$rows")
        if [ "$count" -gt 0 ] && [ -z "$DRY_RUN" ]; then
            if [ "$SCOPE_KIND" != "city" ]; then
                TOTAL_STALE_ISSUES_SKIPPED=$((TOTAL_STALE_ISSUES_SKIPPED + count))
            else
                # close_mode comes from the query's per-row CASE: 'force' when
                # the row carried a non-empty assignee at select time and
                # 'bare' otherwise (see close_ids).
                close_ids_by_mode "closing stale issues" "$rows" "stale:auto-closed by reaper"
                TOTAL_ISSUES_CLOSED=$((TOTAL_ISSUES_CLOSED + CLOSE_IDS_OK))
            fi
        fi
    fi

    # Step 6a: anomaly check — stale open wisp count. Fresh workflow load can
    # legitimately exceed the threshold on busy cities; only old non-message
    # rows indicate a reaper leak.
    if get_sql_count "stale open wisp anomaly" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
        AND issue_type NOT IN ('message')
        AND created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
    "; then
        if [ "$SQL_COUNT_RESULT" -gt "$ALERT_THRESHOLD" ]; then
            scope_anomaly "$SQL_COUNT_RESULT stale open wisps (threshold: $ALERT_THRESHOLD, age: ${MAX_AGE})"
        fi
    fi

    # Step 6b: mail-wisp backlog count, observed separately from reapable wisps.
    if get_sql_count "open mail wisp" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
        AND issue_type = 'message'
    "; then
        TOTAL_MAIL_WISPS=$((TOTAL_MAIL_WISPS + SQL_COUNT_RESULT))
        if [ "$MAIL_ALERT_THRESHOLD" -gt 0 ] && [ "$SQL_COUNT_RESULT" -gt "$MAIL_ALERT_THRESHOLD" ]; then
            scope_anomaly "$SQL_COUNT_RESULT open mail-wisps (mail threshold: $MAIL_ALERT_THRESHOLD)"
        fi
    fi
}

# reap_scope_purge is Step 3 for the current scope.
reap_scope_purge() {
    local out
    local err_file
    local count
    local more
    local batches=0
    local purge_args=(purge "$PURGE_PLANE_FLAG" --older-than "$PURGE_AGE" --json)

    if [ -n "$DRY_RUN" ]; then
        purge_args+=(--dry-run)
    else
        # One purge runs in one transaction, so a large backlog is cleared in
        # bounded batches (oldest closed first) within this run's purge
        # budget; whatever is left is picked up by the next run.
        purge_args+=(--force --limit "$PURGE_BATCH")
    fi
    if [ -z "$PURGE_DEADLINE" ]; then
        PURGE_DEADLINE=$(( $(date +%s) + PURGE_BUDGET_SECS ))
    fi
    err_file=$(mktemp)
    while :; do
        if ! out=$(scope_bd "${purge_args[@]}" 2>"$err_file"); then
            out="$(cat "$err_file" 2>/dev/null) $out"
            case "$out" in
                *"unknown flag: $PURGE_PLANE_FLAG"* | *"unknown flag: --limit"*)
                    # A bd whose purge cannot select the wisps plane or purge
                    # in bounded batches (bd v1.3.0, and builds between). The
                    # store is fine; the step waits for a bd that supports
                    # both, and the run says so without escalating.
                    order_outcome_scope_skipped "$SCOPE_LABEL" "bd-purge-unsupported"
                    rm -f "$err_file"
                    return 0
                    ;;
            esac
            scope_anomaly "purging closed wisps failed for $SCOPE_DB: $(sanitize_output "$out")"
            order_outcome_scope_skipped "$SCOPE_LABEL" "purge failed"
            rm -f "$err_file"
            return 0
        fi
        if [ -n "$DRY_RUN" ]; then
            count=$(printf '%s' "$out" | jq -r '.purge_count // .purged_count // 0' 2>/dev/null || echo 0)
            case "$count" in ''|*[!0-9]*) count=0 ;; esac
            TOTAL_WOULD_PURGE=$((TOTAL_WOULD_PURGE + count))
            break
        fi
        count=$(printf '%s' "$out" | jq -r '.purged_count // 0' 2>/dev/null || echo "")
        case "$count" in
            ''|*[!0-9]*)
                scope_anomaly "gc bd purge returned an unreadable result for $SCOPE_DB: $(sanitize_output "$out")"
                break
                ;;
        esac
        TOTAL_PURGED=$((TOTAL_PURGED + count))
        batches=$((batches + 1))
        # bd reports whether closed wisps are left beyond this batch; a
        # result without that signal is read as "a full batch may have more".
        more=$(printf '%s' "$out" | jq -r --argjson limit "$PURGE_BATCH" '
            if has("has_more") then .has_more
            elif has("remaining") then (.remaining > 0)
            else (.purged_count // 0) >= $limit end' 2>/dev/null || echo false)
        if [ "$more" != "true" ] || [ "$count" -eq 0 ]; then
            break
        fi
        if [ "$(date +%s)" -ge "$PURGE_DEADLINE" ]; then
            order_outcome_scope_skipped "$SCOPE_LABEL" "purge backlog remains after this run's purge budget"
            break
        fi
        run_budget_exhausted && break
    done
    rm -f "$err_file"
}

# Scopes are resolved first so that scopes sharing one database (a rig bound
# to the city store) are reaped once, with every sharing rig's store ref.
# A scope bd cannot reach is recorded and skipped; the others still run.
SCOPE_TABLE=""
resolve_scope_row() {
    scope_select "$@"
    if ! scope_resolve_db; then
        if [ "$SCOPE_NOT_BD" -eq 1 ]; then
            echo "reaper: $SCOPE_LABEL is not a bd bead store; nothing to reap there"
            order_outcome_scope_skipped "$SCOPE_LABEL" "not a bd bead store"
            return 0
        fi
        record_anomaly "$SCOPE_LABEL" "bead store unreachable through gc bd: $(sanitize_output "$SCOPE_LAST_ERROR")"
        order_outcome_scope_skipped "$SCOPE_LABEL" "bead store unreachable"
        return 0
    fi
    SCOPE_TABLE="${SCOPE_TABLE}${SCOPE_DB}|${SCOPE_KIND}|${SCOPE_RIG}
"
}

resolve_scope_row city
if RIG_NAMES=$(core_rig_names); then
    while IFS= read -r rig_name; do
        [ -n "$rig_name" ] || continue
        resolve_scope_row rig "$rig_name"
    done <<< "$RIG_NAMES"
else
    record_anomaly "rigs" "gc rig list failed; rig scopes were not reaped this run"
    order_outcome_scope_skipped "rigs" "rig list unavailable"
fi

# SCOPE_RIG_REFS lists 'rig:<name>' store refs (SQL literals) of every rig
# scope served by the database being reaped.
SCOPE_RIG_REFS=""
while IFS= read -r scope_db; do
    [ -n "$scope_db" ] || continue
    rows=$(printf '%s' "$SCOPE_TABLE" | awk -F'|' -v db="$scope_db" '$1 == db')
    SCOPE_RIG_REFS=$(printf '%s\n' "$rows" | awk -F'|' '$2 == "rig" && $3 ~ /^[A-Za-z0-9_.-]+$/ { printf "%s'"'"'rig:%s'"'"'", sep, $3; sep=", " }')
    if printf '%s\n' "$rows" | awk -F'|' '$2 == "city" { found=1 } END { exit found ? 0 : 1 }'; then
        scope_select city
        CITY_DB="$scope_db"
    else
        scope_select rig "$(printf '%s\n' "$rows" | awk -F'|' 'NR == 1 { print $3 }')"
    fi
    SCOPE_DB="$scope_db"
    if run_budget_exhausted; then
        order_outcome_scope_skipped "$SCOPE_LABEL" "run budget exhausted before this scope"
        continue
    fi
    TOTAL_SCOPES=$((TOTAL_SCOPES + 1))
    reap_scope
done < <(printf '%s' "$SCOPE_TABLE" | awk -F'|' '!seen[$1]++ { print $1 }')

# Step 6: prune closed session beads from the city store.
# GC_REAPER_SESSION_BEAD_PATTERN defaults to 'gm-*' (legacy Gas Manager prefix).
# Set it to the empty string to use the type-safe path (issue_type=session only).
#
# On a split city, agent sessions (gcg-session-*, gcs-*) live in the sqlite
# infra ledger (.gc/store/graph/beads.sqlite), which bd does not serve here.
# Those rows are purged by the daemon wisp GC (purgeClosedInfraSessions) after
# GC_INFRA_SESSION_PURGE_AGE (default 72h), only when the city's sessions class
# is relocated onto that SQLite ledger and wisp_ttl is set; it reads that
# variable from the controller's environment, not this order's. This step's
# own clock stays GC_REAPER_SESSION_PURGE_AGE (default 720h). On an unsplit
# city the wisp GC leaves sessions alone and this step is the only session
# prune.
if [ -n "$CITY_DB" ] && [ -d "$CITY_BEADS_DIR" ]; then
    scope_select city
    SCOPE_DB="$CITY_DB"
    if [ -n "$SESSION_BEAD_PATTERN" ]; then
        # ── gc bd prune path (pattern-configurable) ──────────────────────────────
        SESSION_PRUNE_ANOMALY_SCOPE="session"
        case "$SESSION_BEAD_PATTERN" in
            *-*) SESSION_PRUNE_ANOMALY_SCOPE="${SESSION_BEAD_PATTERN%%-*}" ;;
        esac

        # Backup-age gate: skip bulk prune unless bd reports a backup of the
        # city store newer than the threshold. bd owns the backup (the dolt
        # pack's backup order runs `gc bd backup sync` per scope), so
        # `gc bd backup status` is the evidence; a scope bd cannot back up
        # reports no evidence and the prune waits. Which record decides
        # mirrors doctor's scanBackupFreshness: a scope with a Dolt backup
        # destination configured is judged on its Dolt sync time only (a fresh
        # JSONL-era backup state must not open the gate while the Dolt backup
        # is stale or never ran); only a scope with no Dolt destination is
        # judged on bd's legacy backup state.
        _PRUNE_MAX_AGE="${GC_REAPER_BACKUP_MAX_AGE:-${GC_BACKUP_MAX_AGE_FOR_BULK_DELETE:-86400}}"
        case "$_PRUNE_MAX_AGE" in ''|*[!0-9]*) _PRUNE_MAX_AGE=86400 ;; esac
        _PRUNE_SKIP=0
        _PRUNE_SKIP_REASON=""
        _PRUNE_BACKUP_UNSUPPORTED=0
        _BACKUP_TS=""
        _BACKUP_ERR_FILE=$(mktemp)
        if _BACKUP_STATUS=$(scope_bd backup status --json 2>"$_BACKUP_ERR_FILE"); then
            _BACKUP_TS=$(printf '%s' "$_BACKUP_STATUS" | jq -r '
                (if (.dolt.configured // false) == true
                 then (.dolt.last_sync // empty)
                 else (.backup.timestamp // empty) end)
                | select(type == "string" and . != "" and (startswith("0001-") | not))' 2>/dev/null || true)
            if [ -z "$_BACKUP_TS" ]; then
                _PRUNE_SKIP_REASON="source=bd-backup-status age=absent"
                _PRUNE_SKIP=1
            fi
        else
            _BACKUP_STATUS="$(cat "$_BACKUP_ERR_FILE" 2>/dev/null) $_BACKUP_STATUS"
            case "$_BACKUP_STATUS" in
                *'"proxy.backup.unsupported"'* | *"not supported in proxied-server mode"*)
                    # This bd cannot back up the city scope's transport, so no
                    # backup evidence can exist yet. The prune waits; the run
                    # reports it without escalating every cooldown.
                    _PRUNE_BACKUP_UNSUPPORTED=1
                    ;;
            esac
            _PRUNE_SKIP_REASON="source=bd-backup-status unavailable: $(sanitize_output "$_BACKUP_STATUS")"
            _PRUNE_SKIP=1
        fi
        rm -f "$_BACKUP_ERR_FILE"
        if [ "$_PRUNE_SKIP" -eq 0 ]; then
            case "$_BACKUP_TS" in
                [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*) ;;
                *)
                    _PRUNE_SKIP_REASON="source=bd-backup-status age=unparseable ($_BACKUP_TS)"
                    _PRUNE_SKIP=1
                    ;;
            esac
        fi
        if [ "$_PRUNE_SKIP" -eq 0 ]; then
            # bd timestamps are RFC3339 (possibly with fractional seconds).
            # Truncate to whole seconds, as Step 4's SQL does.
            case "$_BACKUP_TS" in *.*) _BACKUP_TS="${_BACKUP_TS%%.*}Z" ;; esac
            _BACKUP_EPOCH=$(date -u -d "$_BACKUP_TS" '+%s' 2>/dev/null \
                || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$_BACKUP_TS" '+%s' 2>/dev/null \
                || echo "")
            if [ -z "$_BACKUP_EPOCH" ]; then
                _PRUNE_SKIP_REASON="source=bd-backup-status age=unparseable ($_BACKUP_TS)"
                _PRUNE_SKIP=1
            else
                _BACKUP_AGE=$(( $(date -u '+%s') - _BACKUP_EPOCH ))
                if [ "$_BACKUP_AGE" -gt "$_PRUNE_MAX_AGE" ]; then
                    _PRUNE_SKIP_REASON="source=bd-backup-status age=${_BACKUP_AGE}s"
                    _PRUNE_SKIP=1
                fi
            fi
        fi

        if [ "$_PRUNE_BACKUP_UNSUPPORTED" -eq 1 ]; then
            order_outcome_scope_skipped "city" "session prune skipped: bd-backup-unsupported"
        elif [ "$_PRUNE_SKIP" -eq 1 ]; then
            record_anomaly "$SESSION_PRUNE_ANOMALY_SCOPE" "bulk prune skipped: backup stale or absent ($_PRUNE_SKIP_REASON threshold=${_PRUNE_MAX_AGE}s)"
            order_outcome_scope_skipped "city" "session prune skipped: no fresh backup"
        fi

        # Pattern-validity gate: SESSION_BEAD_PATTERN is spliced into a SQL
        # LIKE literal below (type-scope gate) and passed as bd's own --pattern
        # flag. A value outside this charset (e.g. a single quote) would corrupt
        # the guard's own COUNT query -- id LIKE 'x' OR '1'='1' always matches,
        # silently defeating the guard. Fail closed before any SQL is built.
        if [ "$_PRUNE_SKIP" -eq 0 ]; then
            case "$SESSION_BEAD_PATTERN" in
                ''|*[!A-Za-z0-9_.*?-]*)
                    record_anomaly "$SESSION_PRUNE_ANOMALY_SCOPE" "bulk prune skipped: SESSION_BEAD_PATTERN='$SESSION_BEAD_PATTERN' contains characters outside the allowed pattern charset (letters, digits, . _ - * ?); refusing to build SQL from it (type scope guard)"
                    _PRUNE_SKIP=1
                    ;;
            esac
        fi

        # Type-scope gate: bd's prune has no issue_type filter (only ID glob +
        # age), so a pattern like the default gm-* would delete any closed,
        # non-session bead sharing that ID prefix. Count bd's own prune
        # candidates (pattern + closed + older-than) restricted to non-session
        # rows, and fail closed on a nonzero hit or on a count that could not
        # be computed.
        if [ "$_PRUNE_SKIP" -eq 0 ]; then
            # Glob -> SQL LIKE: simple prefix globs only (e.g. gm-*). A
            # literal % or _ in a custom pattern would not round-trip.
            _TYPE_GUARD_LIKE=$(printf '%s' "$SESSION_BEAD_PATTERN" | sed 's/\*/%/g; s/?/_/g')
            _TYPE_GUARD_AGE_H=$(printf '%s' "$SESSION_PURGE_AGE" | sed 's/h$//')
            case "$_TYPE_GUARD_AGE_H" in ''|*[!0-9]*) _TYPE_GUARD_AGE_H=0 ;; esac
            if ! get_sql_count "type scope guard" "
                SELECT COUNT(*) FROM \`$CITY_DB\`.issues
                WHERE id LIKE '$_TYPE_GUARD_LIKE'
                AND status = 'closed'
                AND closed_at < DATE_SUB(NOW(), INTERVAL $_TYPE_GUARD_AGE_H HOUR)
                AND issue_type != 'session'
            "; then
                record_anomaly "$SESSION_PRUNE_ANOMALY_SCOPE" "bulk prune skipped: type-scope guard count could not be computed (type scope guard); set GC_REAPER_SESSION_BEAD_PATTERN=\"\" to use the type-safe session-only path"
                _PRUNE_SKIP=1
            elif [ "$SQL_COUNT_RESULT" -gt 0 ]; then
                record_anomaly "$SESSION_PRUNE_ANOMALY_SCOPE" "bulk prune skipped: $SQL_COUNT_RESULT non-session bead(s) matching pattern=$SESSION_BEAD_PATTERN would be caught by prune (type scope guard); set GC_REAPER_SESSION_BEAD_PATTERN=\"\" to use the type-safe session-only path"
                _PRUNE_SKIP=1
            fi
        fi

        BD_PRUNE_ARGS=(prune --pattern "$SESSION_BEAD_PATTERN" --older-than "$SESSION_PURGE_AGE")
        # Without --force `gc bd prune` refuses (exit 1, "would prune N"), so a dry
        # run asks for bd's own preview instead.
        if [ -z "$DRY_RUN" ]; then BD_PRUNE_ARGS+=(--force); else BD_PRUNE_ARGS+=(--dry-run); fi
        BD_PRUNE_ARGS+=(--json)
        if [ "$_PRUNE_SKIP" -eq 0 ] && run_budget_exhausted; then
            order_outcome_scope_skipped "city" "session prune skipped: run budget exhausted"
            _PRUNE_SKIP=1
        fi
        if [ "$_PRUNE_SKIP" -eq 0 ]; then
            PRUNE_ERR_FILE=$(mktemp)
            PRUNE_COUNT=""
            if PRUNE_JSON=$(scope_bd "${BD_PRUNE_ARGS[@]}" 2>"$PRUNE_ERR_FILE"); then
                PRUNE_COUNT=$(printf '%s' "$PRUNE_JSON" | jq -r '.pruned_count // .prune_count // empty' 2>/dev/null || true)
            fi
            case "$PRUNE_COUNT" in
                ''|*[!0-9]*)
                    record_anomaly "$SESSION_PRUNE_ANOMALY_SCOPE" "session bead prune failed (pattern=$SESSION_BEAD_PATTERN): $(sanitize_output "$(cat "$PRUNE_ERR_FILE" 2>/dev/null) $PRUNE_JSON")"
                    PRUNE_COUNT=0
                    ;;
            esac
            rm -f "$PRUNE_ERR_FILE"
            TOTAL_SESSIONS_PRUNED=$PRUNE_COUNT
            if [ "$PRUNE_COUNT" -gt 1000 ]; then
                record_anomaly "$SESSION_PRUNE_ANOMALY_SCOPE" "$PRUNE_COUNT closed session beads pruned (pattern=$SESSION_BEAD_PATTERN threshold: 1000)"
            fi
        fi
    else
        # ── type-safe path (issue_type=session only) ──────────────────────────
        # Activated when GC_REAPER_SESSION_BEAD_PATTERN="". Selects only rows
        # with issue_type='session' so it cannot prune non-session beads, and
        # deletes them through bd in batches of 500.
        SESSION_AGE_H=$(printf '%s' "$SESSION_PURGE_AGE" | sed 's/h$//')
        case "$SESSION_AGE_H" in ''|*[!0-9]*) SESSION_AGE_H=720 ;; esac
        if [ -n "$DRY_RUN" ]; then
            if get_sql_count "type-safe session prune" "SELECT COUNT(*) FROM \`$CITY_DB\`.issues WHERE issue_type='session' AND status='closed' AND closed_at < DATE_SUB(NOW(), INTERVAL ${SESSION_AGE_H} HOUR)"; then
                TOTAL_SESSIONS_PRUNED=$SQL_COUNT_RESULT
            fi
        else
            TOTAL=0
            while true; do
                get_sql_rows "type-safe session prune" "SELECT id FROM \`$CITY_DB\`.issues WHERE issue_type='session' AND status='closed' AND closed_at < DATE_SUB(NOW(), INTERVAL ${SESSION_AGE_H} HOUR) LIMIT 500" || break
                BATCH_COUNT=$(count_lines "$SQL_ROWS_RESULT")
                [ "$BATCH_COUNT" -gt 0 ] || break
                run_budget_exhausted && break
                SESSION_IDS_FILE=$(mktemp)
                DELETE_ERR_FILE=$(mktemp)
                printf '%s\n' "$SQL_ROWS_RESULT" >"$SESSION_IDS_FILE"
                DELETED_COUNT=""
                if DELETE_OUT=$(scope_bd delete --force --json --from-file "$SESSION_IDS_FILE" 2>"$DELETE_ERR_FILE"); then
                    DELETED_COUNT=$(printf '%s' "$DELETE_OUT" | jq -r '.deleted_count // empty' 2>/dev/null || true)
                fi
                case "$DELETED_COUNT" in
                    ''|*[!0-9]*)
                        record_anomaly "session" "type-safe session prune failed after $TOTAL deletions: $(sanitize_output "$(cat "$DELETE_ERR_FILE" 2>/dev/null) $DELETE_OUT")"
                        rm -f "$SESSION_IDS_FILE" "$DELETE_ERR_FILE"
                        break
                        ;;
                esac
                rm -f "$SESSION_IDS_FILE" "$DELETE_ERR_FILE"
                TOTAL=$((TOTAL + DELETED_COUNT))
                # A batch that deleted nothing would select the same rows again.
                [ "$DELETED_COUNT" -gt 0 ] || break
            done
            TOTAL_SESSIONS_PRUNED=$TOTAL
            if [ "$TOTAL_SESSIONS_PRUNED" -gt 1000 ]; then
                record_anomaly "session" "$TOTAL_SESSIONS_PRUNED closed session beads pruned via the type-safe path (threshold: 1000)"
            fi
        fi
    fi
fi

if [ -d "$CITY_BEADS_DIR" ] && [ -z "$DRY_RUN" ] && command -v gc >/dev/null 2>&1; then
    if SESSION_STATE_PRUNE_JSON=$( (
        cd "$CITY_ABS" && BEADS_DIR="$CITY_BEADS_DIR" gc session prune --state drained --before "$SESSION_STATE_PRUNE_AGE" --json
    ) 2>&1); then
        :
    else
        record_anomaly "gm" "terminal session-state prune failed: $(sanitize_output "$SESSION_STATE_PRUNE_JSON")"
        SESSION_STATE_PRUNE_JSON='{"count":0}'
    fi
    SESSION_STATE_PRUNE_COUNT=$(printf '%s' "$SESSION_STATE_PRUNE_JSON" | sed -n 's/.*"count"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -1)
    [ -z "$SESSION_STATE_PRUNE_COUNT" ] && SESSION_STATE_PRUNE_COUNT=0
    TOTAL_SESSIONS_PRUNED=$((TOTAL_SESSIONS_PRUNED + SESSION_STATE_PRUNE_COUNT))
    if [ "$SESSION_STATE_PRUNE_COUNT" -gt 1000 ]; then
        record_anomaly "gm" "$SESSION_STATE_PRUNE_COUNT terminal session-state beads pruned in one run (threshold: 1000)"
    fi
fi

if [ "$TOTAL_SCOPES" -eq 0 ]; then
    order_outcome_set skipped "no bd bead scope was reaped"
fi
order_outcome_write

# Report.
if [ -n "$ANOMALIES" ]; then
    "$ESCALATE_SCRIPT" \
        --subject "ESCALATION: Reaper anomalies detected [MEDIUM]" \
        --message "$ANOMALIES" 2>/dev/null || true
fi

SUMMARY="reaper — scopes:$TOTAL_SCOPES, stale_wisps:$TOTAL_STALE_WISPS, closed_wisps:$TOTAL_CLOSED_WISPS, held_wisps:$TOTAL_HELD_WISPS, workflow_roots:$TOTAL_WORKFLOW_ROOTS_CLOSED, skipped_cross_store_workflow_roots:$TOTAL_WORKFLOW_ROOTS_STORE_REF_SKIPPED, skipped_non_city_workflow_issue_roots:$TOTAL_WORKFLOW_ISSUE_ROOTS_SKIPPED, purged:$TOTAL_PURGED, sessions-pruned:$TOTAL_SESSIONS_PRUNED, closed:$TOTAL_ISSUES_CLOSED, expired:$TOTAL_EXPIRED_ISSUES_CLOSED, expired_skipped:$TOTAL_EXPIRED_ISSUES_SKIPPED, skipped_non_city_issues:$TOTAL_STALE_ISSUES_SKIPPED, mail_wisps:$TOTAL_MAIL_WISPS"
if [ -n "$DRY_RUN" ]; then
    SUMMARY="$SUMMARY, would_close_wisps:$TOTAL_WOULD_CLOSE_WISPS, would_close_workflow_roots:$TOTAL_WOULD_CLOSE_WORKFLOW_ROOTS, would_purge:$TOTAL_WOULD_PURGE, would_expire:$TOTAL_WOULD_EXPIRE (dry run)"
fi

maintenance_done "$SUMMARY"
echo "reaper: $SUMMARY"
