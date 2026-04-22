#!/usr/bin/env bash
# mol-dog-phantom-db — detect and quarantine phantom Dolt database directories.
#
# Replaces mol-dog-phantom-db formula. All operations are deterministic:
# filesystem scan for .dolt/ dirs without noms/manifest, rm -rf of corrupted
# dirs, escalation mail if any found. No LLM judgment needed.
#
# A phantom database has a .dolt/ subdirectory but no .dolt/noms/manifest.
# Dolt's auto-discovery crashes INFORMATION_SCHEMA on these at startup.
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
PHANTOMS=""
PHANTOM_COUNT=0
VALID=0

for dir in "$DATA_DIR"/*/; do
    [ -d "$dir" ] || continue
    SCANNED=$((SCANNED + 1))
    db_name=$(basename "$dir")
    if [ -d "$dir/.dolt" ] && [ ! -f "$dir/.dolt/noms/manifest" ]; then
        PHANTOMS="$PHANTOMS $db_name"
        PHANTOM_COUNT=$((PHANTOM_COUNT + 1))
    else
        VALID=$((VALID + 1))
    fi
done

if [ "$PHANTOM_COUNT" -eq 0 ]; then
    SUMMARY="phantom-db — scanned: $SCANNED, phantoms: 0, valid: $VALID"
    gc nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
    echo "phantom-db: $SUMMARY"
    exit 0
fi

# --- Step 2: Quarantine phantom databases ---

QUARANTINED=0
ERRORS=0
for db_name in $PHANTOMS; do
    phantom_path="$DATA_DIR/$db_name"
    if [ -d "$phantom_path" ]; then
        if rm -rf "$phantom_path" 2>/dev/null; then
            QUARANTINED=$((QUARANTINED + 1))
        else
            ERRORS=$((ERRORS + 1))
        fi
    fi
done

# Phantom DBs indicate a Dolt bug — always escalate when found.
gc mail send mayor/ \
    -s "ESCALATION: Quarantined phantom databases [HIGH]" \
    -m "Found and quarantined $QUARANTINED phantom database(s) in $DATA_DIR:$PHANTOMS
$([ "$ERRORS" -gt 0 ] && echo "Removal errors: $ERRORS" || true)" \
    2>/dev/null || true

# --- Step 3: Report ---

SUMMARY="phantom-db — scanned: $SCANNED, phantoms: $PHANTOM_COUNT, quarantined: $QUARANTINED"
gc nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "phantom-db: $SUMMARY"
