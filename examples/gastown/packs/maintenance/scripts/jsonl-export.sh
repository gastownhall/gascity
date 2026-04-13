#!/usr/bin/env bash
# jsonl-export — export Dolt databases to JSONL and push to git archive.
#
# Replaces mol-dog-jsonl formula. All operations are deterministic:
# dolt sql exports, wc -l comparisons against spike threshold, git
# add/commit/push. No LLM judgment needed.
#
# NOTE on `bd batch` (beads#6): this script has no `bd` mutation calls.
# All data operations use `dolt sql` and `git` directly.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

CITY="${GC_CITY:-.}"
DOLT_PORT="${GC_DOLT_PORT:-3307}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/maintenance}"
LEGACY_ARCHIVE_REPO="$CITY/.gc/jsonl-archive"
LEGACY_STATE_FILE="$CITY/.gc/jsonl-export-state.json"

# Configurable via environment (defaults match the old formula).
SPIKE_THRESHOLD="${GC_JSONL_SPIKE_THRESHOLD:-20}"  # percentage (0-100)
MAX_PUSH_FAILURES="${GC_JSONL_MAX_PUSH_FAILURES:-3}"
SCRUB="${GC_JSONL_SCRUB:-true}"
ARCHIVE_REPO="${GC_JSONL_ARCHIVE_REPO:-$PACK_STATE_DIR/jsonl-archive}"

# State file for tracking consecutive push failures.
STATE_FILE="$PACK_STATE_DIR/jsonl-export-state.json"

if [ -z "${GC_JSONL_ARCHIVE_REPO:-}" ] && [ ! -d "$ARCHIVE_REPO/.git" ] && [ -d "$LEGACY_ARCHIVE_REPO/.git" ]; then
    ARCHIVE_REPO="$LEGACY_ARCHIVE_REPO"
fi
if [ ! -e "$STATE_FILE" ] && [ -e "$LEGACY_STATE_FILE" ]; then
    STATE_FILE="$LEGACY_STATE_FILE"
fi
mkdir -p "$(dirname "$STATE_FILE")"

# Discover databases.
DATABASES=$(dolt sql -P "$DOLT_PORT" -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2 | grep -v '^information_schema$\|^mysql$' || true)
if [ -z "$DATABASES" ]; then
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
    SCRUB_FILTER="WHERE type NOT IN ('message', 'event', 'wisp', 'agent') AND title NOT LIKE 'gc:%'"
fi

TOTAL_EXPORTED=0
TOTAL_DBS=0
FAILED_DBS=""
HALTED=0

for DB in $DATABASES; do
    TOTAL_DBS=$((TOTAL_DBS + 1))
    DB_DIR="$ARCHIVE_REPO/$DB"
    mkdir -p "$DB_DIR"

    # Step 1: Export issues table.
    if ! dolt sql -P "$DOLT_PORT" -r json -q "SELECT * FROM \`$DB\`.issues $SCRUB_FILTER" > "$DB_DIR/issues.jsonl" 2>/dev/null; then
        FAILED_DBS="${FAILED_DBS}$DB "
        continue
    fi

    # Export supplemental tables (best-effort).
    for TABLE in comments config dependencies labels metadata; do
        dolt sql -P "$DOLT_PORT" -r json -q "SELECT * FROM \`$DB\`.\`$TABLE\`" > "$DB_DIR/$TABLE.jsonl" 2>/dev/null || true
    done

    # Legacy flat file.
    cp "$DB_DIR/issues.jsonl" "$ARCHIVE_REPO/$DB.jsonl" 2>/dev/null || true

    # Count records exported.
    CURRENT_COUNT=$(wc -l < "$DB_DIR/issues.jsonl" 2>/dev/null || echo "0")
    TOTAL_EXPORTED=$((TOTAL_EXPORTED + CURRENT_COUNT))

    # Step 2: Filter test pollution.
    if [ "$SCRUB" = "true" ] && [ -s "$DB_DIR/issues.jsonl" ]; then
        # Remove test patterns in-place. Use a temp file for atomicity.
        TMPFILE=$(mktemp)
        grep -v -E '"title"\s*:\s*"(Test Issue|test_)' "$DB_DIR/issues.jsonl" \
            | grep -v -E '"id"\s*:\s*"(bd-1|bd-abc12|testdb_|beads_t)' \
            > "$TMPFILE" 2>/dev/null || true
        mv "$TMPFILE" "$DB_DIR/issues.jsonl"
    fi

    # Step 3: Spike detection — compare against previous commit.
    PREV_COUNT=0
    if git -C "$ARCHIVE_REPO" log -1 --format=%H -- "$DB/issues.jsonl" >/dev/null 2>&1; then
        PREV_COUNT=$(git -C "$ARCHIVE_REPO" show HEAD:"$DB/issues.jsonl" 2>/dev/null | wc -l || echo "0")
    fi

    if [ "$PREV_COUNT" -gt 0 ]; then
        FILTERED_COUNT=$(wc -l < "$DB_DIR/issues.jsonl" 2>/dev/null || echo "0")
        if [ "$PREV_COUNT" -gt 0 ]; then
            DELTA=$(( (FILTERED_COUNT - PREV_COUNT) * 100 / PREV_COUNT ))
            # Use absolute value.
            if [ "$DELTA" -lt 0 ]; then
                DELTA=$(( -DELTA ))
            fi
            if [ "$DELTA" -gt "$SPIKE_THRESHOLD" ]; then
                gc mail send mayor/ -s "ESCALATION: JSONL spike detected [HIGH]" \
                    -m "Database: $DB, prev: $PREV_COUNT, current: $FILTERED_COUNT, delta: ${DELTA}%, threshold: ${SPIKE_THRESHOLD}%" \
                    2>/dev/null || true
                HALTED=1
                echo "jsonl-export: HALTED — spike in $DB (${DELTA}% > ${SPIKE_THRESHOLD}%)"
                break
            fi
        fi
    fi
done

if [ "$HALTED" -eq 1 ]; then
    gc nudge deacon/ "DOG_DONE: jsonl — HALTED on spike detection" 2>/dev/null || true
    exit 0
fi

# Step 4: Commit and push.
cd "$ARCHIVE_REPO"
git add -A *.jsonl */ 2>/dev/null || true

if git diff --cached --quiet 2>/dev/null; then
    # No changes.
    gc nudge deacon/ "DOG_DONE: jsonl — no changes" 2>/dev/null || true
    exit 0
fi

EXPORTED_DBS=$((TOTAL_DBS - $(echo "$FAILED_DBS" | wc -w)))
git commit -q -m "backup $(date -u +%Y-%m-%dT%H:%M:%SZ): exported=$EXPORTED_DBS/$TOTAL_DBS records=$TOTAL_EXPORTED" \
    --author="Gas Town Daemon <daemon@gastown.local>" 2>/dev/null || true

PUSH_STATUS="ok"
if ! git push origin main -q 2>/dev/null; then
    PUSH_STATUS="failed"

    # Track consecutive failures.
    CONSECUTIVE=0
    if [ -f "$STATE_FILE" ]; then
        CONSECUTIVE=$(jq -r '.consecutive_push_failures // 0' "$STATE_FILE" 2>/dev/null || echo "0")
    fi
    CONSECUTIVE=$((CONSECUTIVE + 1))
    echo "{\"consecutive_push_failures\": $CONSECUTIVE}" > "$STATE_FILE"

    if [ "$CONSECUTIVE" -ge "$MAX_PUSH_FAILURES" ]; then
        gc mail send mayor/ -s "ESCALATION: JSONL push failed [HIGH]" \
            -m "Consecutive failures: $CONSECUTIVE (threshold: $MAX_PUSH_FAILURES)" \
            2>/dev/null || true
    fi
else
    # Reset failure counter on success.
    if [ -f "$STATE_FILE" ]; then
        echo '{"consecutive_push_failures": 0}' > "$STATE_FILE"
    fi
fi

SUMMARY="jsonl — exported $EXPORTED_DBS/$TOTAL_DBS, records: $TOTAL_EXPORTED, push: $PUSH_STATUS"
if [ -n "$FAILED_DBS" ]; then
    SUMMARY="$SUMMARY, failed: $FAILED_DBS"
fi

gc nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "jsonl-export: $SUMMARY"
