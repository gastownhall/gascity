#!/usr/bin/env bash
# jsonl-export — export Dolt databases to JSONL and push to git archive.
#
# Replaces mol-dog-jsonl formula. All operations are deterministic:
# dolt sql exports, jq record-count comparisons against spike threshold,
# git add/commit/push. No LLM judgment needed.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

CITY="${GC_CITY:-.}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/dolt-target.sh"

# jq is a hard dependency: count_jsonl_rows below relies on it, and a missing
# jq would silently zero every record count and could mask spikes on a stale
# baseline. Fail loud at startup instead.
if ! command -v jq >/dev/null 2>&1; then
    echo "jsonl-export: jq is required but not found in PATH" >&2
    exit 1
fi
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/maintenance}"
LEGACY_ARCHIVE_REPO="$CITY/.gc/jsonl-archive"
LEGACY_STATE_FILE="$CITY/.gc/jsonl-export-state.json"

# Configurable via environment (defaults match the old formula).
SPIKE_THRESHOLD="${GC_JSONL_SPIKE_THRESHOLD:-20}"  # percentage (0-100)
# Skip the percentage spike check when the previous record count is below
# this absolute floor — small-N percentages are noise. Set to 0 to disable.
MIN_PREV_FOR_SPIKE_CHECK="${GC_JSONL_MIN_PREV_FOR_SPIKE:-10}"
MAX_PUSH_FAILURES="${GC_JSONL_MAX_PUSH_FAILURES:-3}"
SCRUB="${GC_JSONL_SCRUB:-true}"
ARCHIVE_REPO="${GC_JSONL_ARCHIVE_REPO:-$PACK_STATE_DIR/jsonl-archive}"

# Count records in a `dolt sql -r json` payload. The output is `{"rows":[...]}`
# on (typically) a single physical line, so `wc -l` measures formatting, not
# records. Falls back to 0 on empty/missing/unparseable input; jq parse errors
# are forwarded to stderr so a corrupt archive surfaces in operator logs
# instead of being silently scored as zero rows.
count_jsonl_rows() {
    jq -s -r 'if length == 0 then 0 else ((.[0].rows // []) | length) end' || echo "0"
}

# Scrub test-only rows while preserving the JSON export structure and legitimate
# rows in the same payload. The input is one JSON object with a .rows array, not
# newline-delimited JSON, so row-level filtering must happen inside jq.
scrub_exported_issues() {
    jq -c '
        if (.rows? | type) == "array" then
            .rows |= map(
                select(
                    ((.title // "") | test("^(Test Issue|test_)") | not) and
                    (
                        (
                            (.id // "") == "bd-1" or
                            (.id // "") == "bd-abc12" or
                            ((.id // "") | test("^(testdb_|beads_t)"))
                        ) | not
                    )
                )
            )
        else
            .
        end
    '
}

validate_exported_issues() {
    jq -e -c '
        if (type == "object") and ((.rows? // [] | type) == "array") then
            .
        else
            error("issues export must be a JSON object with a rows array")
        end
    '
}

normalize_pending_spike_alert_state() {
    jq -c '
        (.pending_spike_alerts //= {}) |
        if (.pending_spike_alert? | type) == "object" and ((.pending_spike_alert.database // "") != "") then
            .pending_spike_alerts[.pending_spike_alert.database] = (.pending_spike_alerts[.pending_spike_alert.database] // .pending_spike_alert)
        else
            .
        end |
        del(.pending_spike_alert) |
        if .pending_spike_alerts == {} then
            del(.pending_spike_alerts)
        else
            .
        end
    '
}

read_state_object() {
    local path="$1"

    jq -c '
        if type == "object" then
            .
        else
            error("state root must be a JSON object")
        end
    ' "$path" 2>/dev/null
}

read_state_json() {
    if [ -f "$STATE_FILE" ] && read_state_object "$STATE_FILE"; then
        return
    fi
    if [ -f "$STATE_FILE_BACKUP" ] && read_state_object "$STATE_FILE_BACKUP"; then
        if [ -f "$STATE_FILE" ]; then
            echo "jsonl-export: state file malformed; using last-known-good backup" >&2
        else
            echo "jsonl-export: state file missing; using last-known-good backup" >&2
        fi
        return
    fi
    if [ -f "$STATE_FILE" ]; then
        echo "jsonl-export: state file malformed; resetting to empty state" >&2
    fi
    echo '{}'
}

write_state_file_atomically() {
    local path="$1"
    local label="$2"
    local content="$3"
    local tmpfile

    if ! tmpfile=$(mktemp "${path}.tmp.XXXXXX"); then
        echo "jsonl-export: creating temporary $label failed" >&2
        return 1
    fi
    if ! printf '%s\n' "$content" > "$tmpfile"; then
        echo "jsonl-export: writing temporary $label failed" >&2
        rm -f "$tmpfile"
        return 1
    fi
    if ! mv -f "$tmpfile" "$path"; then
        echo "jsonl-export: replacing $label failed" >&2
        rm -f "$tmpfile"
        return 1
    fi
}

write_state_json() {
    if ! write_state_file_atomically "$STATE_FILE" "state file" "$1"; then
        return 1
    fi
    if ! write_state_file_atomically "$STATE_FILE_BACKUP" "state backup" "$1"; then
        echo "jsonl-export: state backup update failed; continuing with primary state only" >&2
    fi
}

set_consecutive_push_failures() {
    local count="$1"
    write_state_json "$(read_state_json | jq -c --argjson count "$count" '.consecutive_push_failures = $count')"
}

set_pending_archive_push() {
    write_state_json "$(read_state_json | jq -c '.pending_archive_push = true')"
}

clear_pending_archive_push() {
    write_state_json "$(read_state_json | jq -c 'del(.pending_archive_push)')"
}

has_pending_archive_push() {
    [ "$(read_state_json | jq -r '.pending_archive_push // false')" = "true" ]
}

refresh_archive_remote_main() {
    git fetch origin main -q 2>/dev/null
}

archive_has_local_only_commits_from_tracking() {
    local merge_base

    if ! git rev-parse --verify refs/remotes/origin/main >/dev/null 2>&1; then
        return 1
    fi
    merge_base=$(git merge-base refs/remotes/origin/main HEAD 2>/dev/null) || return 1
    [ "$(git rev-list --count "$merge_base..HEAD" 2>/dev/null || echo "0")" -gt 0 ]
}

archive_has_local_only_commits() {
    if refresh_archive_remote_main >/dev/null 2>&1; then
        archive_has_local_only_commits_from_tracking
        return
    fi
    if archive_has_local_only_commits_from_tracking; then
        echo "jsonl-export: fetch failed while checking deferred archive push; using existing origin/main tracking ref" >&2
        return 0
    fi
    return 1
}

# Detect the archive's push mode from the live state of its remotes rather
# than from a cached state field. Operators opt into off-box backup by adding
# an `origin` remote; removing it reverts to local-only on the next run with
# no extra command.
get_archive_mode() {
    if [ -d "$ARCHIVE_REPO/.git" ] \
        && git -C "$ARCHIVE_REPO" remote get-url origin >/dev/null 2>&1; then
        echo "push"
    else
        echo "local-only"
    fi
}

should_attempt_push() {
    [ "$(get_archive_mode)" = "push" ]
}

# Log the archive mode on transitions and re-log weekly so operators who
# missed the first line still see the current configuration. State fields
# last_logged_mode and last_logged_at drive the re-log interval.
log_archive_mode_if_needed() {
    local current_mode
    local state_json
    local last_logged_mode
    local last_logged_at
    local now
    local now_ts
    local last_ts
    local should_log=0
    local message

    current_mode=$(get_archive_mode)
    state_json=$(read_state_json)
    last_logged_mode=$(printf '%s\n' "$state_json" | jq -r '.last_logged_mode // empty')
    last_logged_at=$(printf '%s\n' "$state_json" | jq -r '.last_logged_at // empty')
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    now_ts=$(date -u +%s)

    if [ "$current_mode" != "$last_logged_mode" ]; then
        should_log=1
    elif [ -z "$last_logged_at" ]; then
        should_log=1
    else
        last_ts=$(jq -n -r --arg ts "$last_logged_at" '$ts | try fromdateiso8601 catch 0')
        if [ "$last_ts" = "0" ] || [ "$((now_ts - last_ts))" -gt 604800 ]; then
            should_log=1
        fi
    fi

    if [ "$should_log" -eq 0 ]; then
        return 0
    fi

    if [ "$current_mode" = "push" ]; then
        message="jsonl-export: archive running in push mode (origin configured; will push commits to remote)"
    else
        message="jsonl-export: archive running in local-only mode (no origin remote; commits stay on this host — off-box backup disabled)"
    fi
    echo "$message" >&2

    # On entering local-only mode, clear consecutive_push_failures so a later
    # return to push mode starts from a clean counter. Without this, a
    # push→local-only→push round-trip (operator removes then re-adds origin)
    # would carry the old failure count forward and could trigger a premature
    # HIGH escalation on the very first failure after origin returns.
    # pending_archive_push is intentionally NOT cleared here — it correctly
    # tracks that local commits still need to be pushed once origin returns.
    # shellcheck disable=SC2016  # $mode/$at are jq variables, not bash
    local jq_filter='.last_logged_mode = $mode | .last_logged_at = $at'
    if [ "$current_mode" = "local-only" ]; then
        jq_filter="$jq_filter | .consecutive_push_failures = 0"
    fi

    write_state_json "$(
        printf '%s\n' "$state_json" \
            | jq -c --arg mode "$current_mode" --arg at "$now" "$jq_filter"
    )"
}

set_pending_spike_alert() {
    local db="$1"
    local prev_count="$2"
    local current_count="$3"
    local delta="$4"
    local threshold="$5"

    write_state_json "$(
        read_state_json \
            | normalize_pending_spike_alert_state \
            | jq -c \
            --arg db "$db" \
            --argjson prev_count "$prev_count" \
            --argjson current_count "$current_count" \
            --argjson delta "$delta" \
            --argjson threshold "$threshold" \
            '.pending_spike_alerts[$db] = {
                database: $db,
                prev_count: $prev_count,
                current_count: $current_count,
                delta: $delta,
                threshold: $threshold
            }'
    )"
}

clear_pending_spike_alert() {
    local db="${1:-}"

    if [ -z "$db" ]; then
        write_state_json "$(read_state_json | jq -c 'del(.pending_spike_alert, .pending_spike_alerts)')"
        return
    fi

    write_state_json "$(
        read_state_json \
            | normalize_pending_spike_alert_state \
            | jq -c --arg db "$db" '
                del(.pending_spike_alerts[$db]) |
                if (.pending_spike_alerts // {}) == {} then
                    del(.pending_spike_alerts)
                else
                    .
                end
            '
    )"
}

send_spike_alert() {
    local db="$1"
    local prev_count="$2"
    local current_count="$3"
    local delta="$4"
    local threshold="$5"

    gc mail send mayor/ -s "ESCALATION: JSONL spike detected [HIGH]" \
        -m "Database: $db, prev: $prev_count, current: $current_count, delta: ${delta}%, threshold: ${threshold}%" \
        2>/dev/null
}

retry_pending_spike_alert() {
    local state_json
    local updated_state_json
    local state_changed=0
    local alert_json
    local pending_alerts=()
    local db
    local prev_count
    local current_count
    local delta
    local threshold

    state_json=$(read_state_json | normalize_pending_spike_alert_state)
    updated_state_json="$state_json"
    while IFS= read -r alert_json; do
        [ -n "$alert_json" ] || continue
        pending_alerts+=("$alert_json")
    done < <(
        printf '%s\n' "$state_json" \
            | jq -c '.pending_spike_alerts // {} | to_entries | sort_by(.key) | .[].value'
    )
    if [ "${#pending_alerts[@]}" -eq 0 ]; then
        return
    fi

    for alert_json in "${pending_alerts[@]}"; do
        db=$(printf '%s\n' "$alert_json" | jq -r '.database // empty')
        if [ -z "$db" ]; then
            continue
        fi
        prev_count=$(printf '%s\n' "$alert_json" | jq -r '.prev_count // 0')
        current_count=$(printf '%s\n' "$alert_json" | jq -r '.current_count // 0')
        delta=$(printf '%s\n' "$alert_json" | jq -r '.delta // 0')
        threshold=$(printf '%s\n' "$alert_json" | jq -r '.threshold // 0')

        if send_spike_alert "$db" "$prev_count" "$current_count" "$delta" "$threshold"; then
            updated_state_json=$(
                printf '%s\n' "$updated_state_json" \
                    | jq -c --arg db "$db" '
                        del(.pending_spike_alerts[$db]) |
                        if (.pending_spike_alerts // {}) == {} then
                            del(.pending_spike_alerts)
                        else
                            .
                        end
                    '
            )
            state_changed=1
            continue
        fi
        echo "jsonl-export: pending spike alert delivery failed for $db" >&2
    done

    if [ "$state_changed" -eq 1 ]; then
        write_state_json "$updated_state_json"
    fi
}

push_archive_main() {
    local consecutive
    local fetch_err
    local rebase_err
    local push_err

    # Retain only the last ~20 lines of stderr so an extremely chatty failure
    # doesn't drown the escalation body.
    truncate_stderr_context() {
        local raw="$1"

        [ -z "$raw" ] && return 0
        printf '%s\n' "$raw" | tail -n 20
    }

    record_archive_push_failure() {
        local message="$1"
        local stderr_context="$2"
        local body
        local stderr_display

        echo "$message" >&2
        consecutive=$(read_state_json | jq -r '.consecutive_push_failures // 0' || echo "0")
        consecutive=$((consecutive + 1))
        set_consecutive_push_failures "$consecutive"
        set_pending_archive_push

        if [ "$consecutive" -ge "$MAX_PUSH_FAILURES" ]; then
            stderr_display=$(truncate_stderr_context "$stderr_context")
            if [ -z "$stderr_display" ]; then
                stderr_display="(no stderr captured)"
            fi
            body=$(cat <<ESCALATION
Order: mol-dog-jsonl
Archive: $ARCHIVE_REPO
Consecutive failures: $consecutive (threshold: $MAX_PUSH_FAILURES)

Last git push stderr:
$stderr_display

Remediation:
- Check remote: git -C $ARCHIVE_REPO remote -v
- Verify remote is reachable and credentials are valid
- Temporarily suppress: export GC_JSONL_MAX_PUSH_FAILURES=99
- See docs/getting-started/troubleshooting.md#jsonl-archive-push-failures
ESCALATION
)
            gc mail send mayor/ -s "ESCALATION: JSONL push failed [HIGH]" \
                -m "$body" \
                2>/dev/null || true
        fi

        return 1
    }

    # Branch on the actual git exit status, not on whether stderr is non-empty.
    # Successful git commands can emit benign stderr (e.g. "warning: redirecting
    # to https://...", credential-helper notes, protocol upgrade hints) which
    # would otherwise misclassify the run as a failure and falsely escalate.
    if ! fetch_err=$(git fetch origin main -q 2>&1 >/dev/null); then
        if git rev-parse --verify refs/remotes/origin/main >/dev/null 2>&1; then
            record_archive_push_failure \
                "jsonl-export: fetching origin/main failed" \
                "$fetch_err"
            return 1
        fi
        echo "jsonl-export: origin/main missing; attempting initial push bootstrap" >&2
    fi

    if git rev-parse --verify refs/remotes/origin/main >/dev/null 2>&1; then
        if ! git merge-base --is-ancestor refs/remotes/origin/main HEAD >/dev/null 2>&1; then
            if ! rebase_err=$(git rebase refs/remotes/origin/main 2>&1 >/dev/null); then
                git rebase --abort >/dev/null 2>&1 || true
                record_archive_push_failure \
                    "jsonl-export: rebase onto origin/main failed during archive push recovery" \
                    "$rebase_err"
                return 1
            fi
        fi
        if ! archive_has_local_only_commits_from_tracking; then
            set_consecutive_push_failures "0"
            clear_pending_archive_push
            return 0
        fi
    fi

    if ! push_err=$(git push origin main -q 2>&1 >/dev/null); then
        record_archive_push_failure \
            "jsonl-export: pushing archive main failed" \
            "$push_err"
        return 1
    fi

    set_consecutive_push_failures "0"
    clear_pending_archive_push
    return 0
}

commit_archive_snapshot() {
    local message="$1"
    local context="$2"

    if ! GIT_AUTHOR_NAME="Gas Town Daemon" \
        GIT_AUTHOR_EMAIL="daemon@gastown.local" \
        GIT_COMMITTER_NAME="Gas Town Daemon" \
        GIT_COMMITTER_EMAIL="daemon@gastown.local" \
        git commit -q -m "$message"; then
        echo "jsonl-export: $context commit failed" >&2
        return 1
    fi
}

discard_failed_db_outputs() {
    local db="$1"

    rm -rf "$ARCHIVE_REPO/$db"
    rm -f "$ARCHIVE_REPO/$db.jsonl"

    if git -C "$ARCHIVE_REPO" cat-file -e "HEAD:$db/issues.jsonl" 2>/dev/null; then
        git -C "$ARCHIVE_REPO" restore --source=HEAD --worktree -- "$db" >/dev/null 2>&1 || true
    fi
    if git -C "$ARCHIVE_REPO" cat-file -e "HEAD:$db.jsonl" 2>/dev/null; then
        git -C "$ARCHIVE_REPO" restore --source=HEAD --worktree -- "$db.jsonl" >/dev/null 2>&1 || true
    fi
}

discard_staged_archive_outputs() {
    local path

    if [ "${#STAGE_PATHS[@]}" -eq 0 ]; then
        return
    fi

    git reset -q -- "${STAGE_PATHS[@]}" >/dev/null 2>&1 || true
    for path in "${STAGE_PATHS[@]}"; do
        if git cat-file -e "HEAD:$path" 2>/dev/null; then
            git restore --source=HEAD --staged --worktree -- "$path" >/dev/null 2>&1 || true
            git clean -fd -- "$path" >/dev/null 2>&1 || true
            continue
        fi
        rm -rf "$path"
    done
}

# State file for tracking consecutive push failures.
STATE_FILE="$PACK_STATE_DIR/jsonl-export-state.json"

if [ -z "${GC_JSONL_ARCHIVE_REPO:-}" ] && [ ! -d "$ARCHIVE_REPO/.git" ] && [ -d "$LEGACY_ARCHIVE_REPO/.git" ]; then
    ARCHIVE_REPO="$LEGACY_ARCHIVE_REPO"
fi
if [ ! -e "$STATE_FILE" ] && [ -e "$LEGACY_STATE_FILE" ]; then
    STATE_FILE="$LEGACY_STATE_FILE"
fi
STATE_FILE_BACKUP="${STATE_FILE}.bak"
mkdir -p "$(dirname "$STATE_FILE")"

log_archive_mode_if_needed
retry_pending_spike_alert

is_user_database() {
    case "$1" in
        information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe|benchdb|testdb_*|beads_pt*|beads_vr*|doctest_*|doctortest_*)
            return 1
            ;;
        beads_t*)
            local suffix="${1#beads_t}"
            if [[ "$suffix" =~ ^[0-9a-f]{8,}$ ]]; then
                return 1
            fi
            return 0
            ;;
        *)
            return 0
            ;;
    esac
}

# Discover databases. Exclude Dolt/MySQL system schemas, Gas City's internal
# health-probe database, and test-fixture scratch databases (benchdb,
# testdb_*, lowercase beads_t[0-9a-f]{8,}, beads_pt*, beads_vr*,
# doctest_*, doctortest_* — matching the Go cleanup planner contract); the
# remaining databases are expected to be bead stores.
DATABASES=$(
    while IFS= read -r db; do
        if is_user_database "$db"; then
            printf '%s\n' "$db"
        fi
    done < <(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2)
)
if [ -z "$DATABASES" ]; then
    if [ -d "$ARCHIVE_REPO/.git" ]; then
        cd "$ARCHIVE_REPO"
        if has_pending_archive_push || archive_has_local_only_commits; then
            if should_attempt_push; then
                PUSH_STATUS="ok"
                if ! push_archive_main; then
                    PUSH_STATUS="failed"
                fi
            else
                PUSH_STATUS="skipped (local-only)"
            fi
            SUMMARY="jsonl — no user databases, push: $PUSH_STATUS"
            gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
            echo "jsonl-export: $SUMMARY"
        fi
    fi
    exit 0
fi

# Ensure archive repo exists.
if [ ! -d "$ARCHIVE_REPO/.git" ]; then
    mkdir -p "$ARCHIVE_REPO"
    git -C "$ARCHIVE_REPO" init -q 2>/dev/null || true
fi

# Build scrub filter for the issues table.
SCRUB_FILTER=""
if [ "$SCRUB" = "true" ]; then
    SCRUB_FILTER="WHERE issue_type NOT IN ('message', 'event', 'wisp', 'agent') AND title NOT LIKE 'gc:%'"
fi

TOTAL_EXPORTED=0
TOTAL_DBS=0
FAILED_DBS=""
FAILED_DB_COUNT=0
HALTED=0
STAGE_PATHS=()
HALT_DB=""
HALT_PREV_COUNT=0
HALT_CURRENT_COUNT=0
HALT_DELTA=0

valid_database_identifier() {
    local name="$1"

    case "$name" in
        ''|-*|*[!A-Za-z0-9_-]*)
            return 1
            ;;
    esac

    return 0
}

while IFS= read -r DB; do
    [ -z "$DB" ] && continue
    TOTAL_DBS=$((TOTAL_DBS + 1))
    if ! valid_database_identifier "$DB"; then
        FAILED_DB_COUNT=$((FAILED_DB_COUNT + 1))
        FAILED_DBS="${FAILED_DBS}$DB
"
        continue
    fi
    if ! has_wisps_table "$DB"; then
        # Not a bd-managed bead store. Decrement the count we just
        # bumped — schemaless DBs aren't part of the export universe
        # and shouldn't appear in TOTAL_DBS or FAILED_DBS summaries.
        # See dolt-target.sh:has_wisps_table and gastownhall/gascity#1816.
        TOTAL_DBS=$((TOTAL_DBS - 1))
        continue
    fi

    DB_DIR="$ARCHIVE_REPO/$DB"
    mkdir -p "$DB_DIR"

    # Step 1: Export issues table.
    ISSUE_EXPORT_TMP=$(mktemp "$DB_DIR/issues.jsonl.tmp.XXXXXX")
    if ! dolt_sql -r json -q "SELECT * FROM \`$DB\`.issues $SCRUB_FILTER" > "$ISSUE_EXPORT_TMP" 2>/dev/null; then
        rm -f "$ISSUE_EXPORT_TMP"
        discard_failed_db_outputs "$DB"
        FAILED_DB_COUNT=$((FAILED_DB_COUNT + 1))
        FAILED_DBS="${FAILED_DBS}$DB
"
        continue
    fi
    if ! mv -f "$ISSUE_EXPORT_TMP" "$DB_DIR/issues.jsonl"; then
        rm -f "$ISSUE_EXPORT_TMP"
        discard_failed_db_outputs "$DB"
        FAILED_DB_COUNT=$((FAILED_DB_COUNT + 1))
        FAILED_DBS="${FAILED_DBS}$DB
"
        continue
    fi

    # Export supplemental tables (best-effort).
    for TABLE in comments config dependencies labels metadata; do
        dolt_sql -r json -q "SELECT * FROM \`$DB\`.\`$TABLE\`" > "$DB_DIR/$TABLE.jsonl" 2>/dev/null || true
    done

    # Step 2: Validate the exported JSON payload and optionally scrub it. Even
    # when SCRUB=false we still fail the DB on malformed JSON so corrupt live
    # exports cannot silently score as zero rows and become the new baseline.
    TMPFILE=$(mktemp)
    if [ "$SCRUB" = "true" ]; then
        if ! scrub_exported_issues < "$DB_DIR/issues.jsonl" > "$TMPFILE"; then
            rm -f "$TMPFILE"
            discard_failed_db_outputs "$DB"
            FAILED_DB_COUNT=$((FAILED_DB_COUNT + 1))
            FAILED_DBS="${FAILED_DBS}$DB
"
            continue
        fi
    elif ! validate_exported_issues < "$DB_DIR/issues.jsonl" > "$TMPFILE"; then
        rm -f "$TMPFILE"
        discard_failed_db_outputs "$DB"
        FAILED_DB_COUNT=$((FAILED_DB_COUNT + 1))
        FAILED_DBS="${FAILED_DBS}$DB
"
        continue
    fi
    if [ ! -s "$TMPFILE" ]; then
        echo "jsonl-export: issues export for $DB was empty" >&2
        rm -f "$TMPFILE"
        discard_failed_db_outputs "$DB"
        FAILED_DB_COUNT=$((FAILED_DB_COUNT + 1))
        FAILED_DBS="${FAILED_DBS}$DB
"
        continue
    fi
    if ! validate_exported_issues < "$TMPFILE" >/dev/null; then
        rm -f "$TMPFILE"
        discard_failed_db_outputs "$DB"
        FAILED_DB_COUNT=$((FAILED_DB_COUNT + 1))
        FAILED_DBS="${FAILED_DBS}$DB
"
        continue
    fi
    mv -f "$TMPFILE" "$DB_DIR/issues.jsonl"

    # Legacy flat file mirrors the scrubbed per-db export. Keep the two output
    # shapes in sync so any downstream reader sees the same filtered payload.
    if ! cp -f "$DB_DIR/issues.jsonl" "$ARCHIVE_REPO/$DB.jsonl" 2>/dev/null; then
        discard_failed_db_outputs "$DB"
        FAILED_DB_COUNT=$((FAILED_DB_COUNT + 1))
        FAILED_DBS="${FAILED_DBS}$DB
"
        continue
    fi

    # Count records from the final persisted payload (post-scrub / post-
    # validation) so commit messages and DOG_DONE summaries reflect what was
    # actually archived, not the pre-scrub raw export.
    CURRENT_COUNT=$(count_jsonl_rows < "$DB_DIR/issues.jsonl")
    TOTAL_EXPORTED=$((TOTAL_EXPORTED + CURRENT_COUNT))

    STAGE_PATHS+=("$DB" "$DB.jsonl")

    # Step 3: Spike detection — compare record counts against previous commit.
    PREV_COUNT=0
    if git -C "$ARCHIVE_REPO" cat-file -e "HEAD:$DB/issues.jsonl" 2>/dev/null; then
        PREV_COUNT=$(git -C "$ARCHIVE_REPO" show "HEAD:$DB/issues.jsonl" 2>/dev/null | count_jsonl_rows || echo "0")
    fi

    # Skip the percentage check on the first run (no prior commit) and when
    # the previous count is below the absolute floor — a 1→2 swing is 100% but
    # meaningless on a tiny database. The PREV_COUNT > 0 guard also avoids the
    # division-by-zero on line `DELTA=...` when the floor is set to 0 to
    # disable the small-N skip.
    if [ "$PREV_COUNT" -gt 0 ] && [ "$PREV_COUNT" -ge "$MIN_PREV_FOR_SPIKE_CHECK" ]; then
        FILTERED_COUNT=$(count_jsonl_rows < "$DB_DIR/issues.jsonl")
        DELTA=$(( (FILTERED_COUNT - PREV_COUNT) * 100 / PREV_COUNT ))
        if [ "$DELTA" -lt 0 ]; then
            DELTA=$(( -DELTA ))
        fi
        if [ "$DELTA" -gt "$SPIKE_THRESHOLD" ]; then
            HALTED=1
            HALT_DB="$DB"
            HALT_PREV_COUNT="$PREV_COUNT"
            HALT_CURRENT_COUNT="$FILTERED_COUNT"
            HALT_DELTA="$DELTA"
            echo "jsonl-export: HALTED — spike in $DB (${DELTA}% > ${SPIKE_THRESHOLD}%)"
            break
        fi
    fi
done <<EOF
$DATABASES
EOF

cd "$ARCHIVE_REPO"
if [ "${#STAGE_PATHS[@]}" -gt 0 ]; then
    if ! git add -A -- "${STAGE_PATHS[@]}"; then
        discard_staged_archive_outputs
        echo "jsonl-export: staging archive outputs failed" >&2
        exit 1
    fi
fi

# On HALT we still commit the new export so PREV_COUNT advances on the next
# run — otherwise the same spike re-fires every cooldown and floods the inbox
# (#1547 root cause #3). Push is skipped, so the spike snapshot stays local
# until a later successful non-HALT run pushes the archive forward.
if [ "$HALTED" -eq 1 ]; then
    if ! git diff --cached --quiet 2>/dev/null; then
        EXPORTED_DBS=$((TOTAL_DBS - FAILED_DB_COUNT))
        commit_archive_snapshot \
            "[HALT] backup $(date -u +%Y-%m-%dT%H:%M:%SZ): exported=$EXPORTED_DBS/$TOTAL_DBS records=$TOTAL_EXPORTED (spike detected; push skipped)" \
            "HALT baseline" || {
            discard_staged_archive_outputs
            exit 1
        }
        set_pending_archive_push
    fi
    set_pending_spike_alert "$HALT_DB" "$HALT_PREV_COUNT" "$HALT_CURRENT_COUNT" "$HALT_DELTA" "$SPIKE_THRESHOLD"
    if send_spike_alert "$HALT_DB" "$HALT_PREV_COUNT" "$HALT_CURRENT_COUNT" "$HALT_DELTA" "$SPIKE_THRESHOLD"; then
        clear_pending_spike_alert "$HALT_DB"
    else
        echo "jsonl-export: spike alert delivery failed; will retry from state" >&2
    fi
    gc session nudge deacon/ "DOG_DONE: jsonl — HALTED on spike detection" 2>/dev/null || true
    exit 0
fi

if git diff --cached --quiet 2>/dev/null; then
    if has_pending_archive_push || archive_has_local_only_commits; then
        if should_attempt_push; then
            PUSH_STATUS="ok"
            if ! push_archive_main; then
                PUSH_STATUS="failed"
            fi
        else
            PUSH_STATUS="skipped (local-only)"
        fi
        if [ -n "$FAILED_DBS" ]; then
            EXPORTED_DBS=$((TOTAL_DBS - FAILED_DB_COUNT))
            SUMMARY="jsonl — exported $EXPORTED_DBS/$TOTAL_DBS, records: $TOTAL_EXPORTED, push: $PUSH_STATUS, failed: $(printf '%s' "$FAILED_DBS" | tr '\n' ' ')"
        else
            SUMMARY="jsonl — no changes, push: $PUSH_STATUS"
        fi
        gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
        echo "jsonl-export: $SUMMARY"
        exit 0
    fi
    if [ -n "$FAILED_DBS" ]; then
        EXPORTED_DBS=$((TOTAL_DBS - FAILED_DB_COUNT))
        SUMMARY="jsonl — exported $EXPORTED_DBS/$TOTAL_DBS, records: $TOTAL_EXPORTED, push: skipped, failed: $(printf '%s' "$FAILED_DBS" | tr '\n' ' ')"
        gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
        echo "jsonl-export: $SUMMARY"
        exit 0
    fi
    # No changes.
    gc session nudge deacon/ "DOG_DONE: jsonl — no changes" 2>/dev/null || true
    exit 0
fi

EXPORTED_DBS=$((TOTAL_DBS - FAILED_DB_COUNT))
commit_archive_snapshot \
    "backup $(date -u +%Y-%m-%dT%H:%M:%SZ): exported=$EXPORTED_DBS/$TOTAL_DBS records=$TOTAL_EXPORTED" \
    "archive snapshot" || {
    discard_staged_archive_outputs
    exit 1
}
set_pending_archive_push

if should_attempt_push; then
    PUSH_STATUS="ok"
    if ! push_archive_main; then
        PUSH_STATUS="failed"
    fi
else
    PUSH_STATUS="skipped (local-only)"
fi

SUMMARY="jsonl — exported $EXPORTED_DBS/$TOTAL_DBS, records: $TOTAL_EXPORTED, push: $PUSH_STATUS"
if [ -n "$FAILED_DBS" ]; then
    SUMMARY="$SUMMARY, failed: $(printf '%s' "$FAILED_DBS" | tr '\n' ' ')"
fi

gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "jsonl-export: $SUMMARY"
