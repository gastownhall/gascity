#!/usr/bin/env bash
# mol-dog-phantom-db — detect and quarantine phantom Dolt database directories.
#
# Replaces mol-dog-phantom-db formula. All operations are deterministic:
# filesystem scan for unservable .dolt/ dirs, quarantine by move, escalation
# mail if any found. No LLM judgment needed.
#
# A phantom database has a .dolt/ subdirectory but no .dolt/noms/manifest.
# Dolt's auto-discovery crashes INFORMATION_SCHEMA on these at startup.
# A retired replacement database has a .dolt/ subdirectory and a basename
# matching *.replaced-YYYYMMDDTHHMMSSZ.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

DATA_DIR="${GC_PHANTOM_DATA_DIR:-$DOLT_DATA_DIR}"

# --- Step 1: Scan for phantom database directories ---

if [ ! -d "$DATA_DIR" ]; then
    echo "phantom-db: data dir $DATA_DIR not found, skipping"
    exit 0
fi

SCANNED=0
UNSERVABLES=""
PHANTOM_COUNT=0
RETIRED_COUNT=0
UNSERVABLE_COUNT=0
VALID=0

for dir in "$DATA_DIR"/*/; do
    [ -d "$dir" ] || continue
    db_name=$(basename "$dir")
    if [ "$db_name" = ".quarantine" ]; then
        continue
    fi
    SCANNED=$((SCANNED + 1))
    is_unservable=0
    if [ -d "$dir/.dolt" ]; then
        if [ ! -f "$dir/.dolt/noms/manifest" ]; then
            PHANTOM_COUNT=$((PHANTOM_COUNT + 1))
            is_unservable=1
        fi
        case "$db_name" in
            *.replaced-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z)
                RETIRED_COUNT=$((RETIRED_COUNT + 1))
                is_unservable=1
                ;;
        esac
    fi
    if [ "$is_unservable" -eq 1 ]; then
        UNSERVABLES="$UNSERVABLES $db_name"
        UNSERVABLE_COUNT=$((UNSERVABLE_COUNT + 1))
    else
        VALID=$((VALID + 1))
    fi
done

if [ "$UNSERVABLE_COUNT" -eq 0 ]; then
    SUMMARY="phantom-db — scanned: $SCANNED, phantoms: 0, retired: 0, valid: $VALID"
    gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
    echo "phantom-db: $SUMMARY"
    exit 0
fi

# --- Step 2: Quarantine unservable databases ---

QUARANTINED=0
ERRORS=0
QUARANTINE_DIR="$DATA_DIR/.quarantine"
mkdir -p "$QUARANTINE_DIR"
QUARANTINE_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
for db_name in $UNSERVABLES; do
    source_path="$DATA_DIR/$db_name"
    quarantine_path="$QUARANTINE_DIR/${QUARANTINE_STAMP}-$db_name"
    if [ -d "$source_path" ]; then
        if mv -f "$source_path" "$quarantine_path" 2>/dev/null; then
            QUARANTINED=$((QUARANTINED + 1))
        else
            ERRORS=$((ERRORS + 1))
        fi
    fi
done

# Unservable DBs indicate a Dolt bug or failed replacement — always escalate when found.
gc mail send mayor/ \
    -s "ESCALATION: Quarantined unservable databases [HIGH]" \
    -m "Found and quarantined $QUARANTINED unservable database(s) in $DATA_DIR:$UNSERVABLES
$([ "$ERRORS" -gt 0 ] && echo "Quarantine errors: $ERRORS" || true)" \
    2>/dev/null || true

# --- Step 3: Report ---

SUMMARY="phantom-db — scanned: $SCANNED, phantoms: $PHANTOM_COUNT, retired: $RETIRED_COUNT, quarantined: $QUARANTINED"
gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "phantom-db: $SUMMARY"
