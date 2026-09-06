#!/usr/bin/env bash
# Test: reaper rig_name_for_db() — the RIG_STORE_REFS_BY_DB lookup added for
# gastownhall/gascity#5285 (expired rig-scoped nudge shadows evade the
# reaper). This function is what lets Step 4 tell a rig-scoped bead store
# apart from an unbound/undiscoverable one.
#
# Acceptance criteria:
#   1. No entries at all              → not found (rc=1, no output)
#   2. One entry, matching db         → found, prints the rig name
#   3. One entry, non-matching db     → not found (rc=1, no output)
#   4. Multiple entries, second matches → found, prints the right rig name
#   5. Entry present but not "rig:"-prefixed → not found (defensive: only
#      rig: store refs are ever appended today, but the lookup must not
#      misinterpret a future non-rig store_ref as a rig name)
#   6. db is a literal substring of another entry's db → not found for the
#      substring (exact match only, no accidental prefix match)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REAPER="$SCRIPT_DIR/../internal/bootstrap/packs/core/assets/scripts/reaper.sh"
FAILED=0

pass() { printf '\033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }

if [ ! -f "$REAPER" ]; then
    printf 'ERROR: reaper.sh not found at %s\n' "$REAPER" >&2
    exit 1
fi

# Extract the rig_name_for_db function body verbatim from reaper.sh so the
# test exercises the real implementation rather than a re-typed copy.
FUNC=$(awk '
  /^rig_name_for_db\(\)/{found=1}
  found{print}
  found && /^}$/{exit}
' "$REAPER")

if [ -z "$FUNC" ]; then
    printf 'ERROR: could not extract rig_name_for_db() from %s\n' "$REAPER" >&2
    exit 1
fi

# run_lookup <RIG_STORE_REFS_BY_DB> <db>
# Returns: <rc>|<output>
run_lookup() {
    local refs="$1"
    local db="$2"
    local tmpdir run_script rc out
    tmpdir=$(mktemp -d)
    run_script="$tmpdir/run.sh"

    {
        printf '%s\n' "$FUNC"
        printf 'RIG_STORE_REFS_BY_DB=%q\n' "$refs"
        printf 'rig_name_for_db %q\n' "$db"
    } > "$run_script"

    rc=0
    out=$(bash "$run_script" 2>/dev/null) || rc=$?
    rm -rf "$tmpdir"
    printf '%s|%s\n' "$rc" "$out"
}

# ── T1: no entries → not found ────────────────────────────────────────────
result=$(run_lookup "" "rig_alpha_db")
rc=$(printf '%s' "$result" | cut -d'|' -f1)
out=$(printf '%s' "$result" | cut -d'|' -f2-)
if [ "$rc" -ne 0 ] && [ -z "$out" ]; then
    pass "T1: empty map → not found"
else
    fail "T1: empty map → expected rc!=0 and empty output; got rc=$rc out=$out"
fi

# ── T2: one entry, matching db → found ────────────────────────────────────
result=$(run_lookup $'rig_alpha_db|rig:alpha\n' "rig_alpha_db")
rc=$(printf '%s' "$result" | cut -d'|' -f1)
out=$(printf '%s' "$result" | cut -d'|' -f2-)
if [ "$rc" -eq 0 ] && [ "$out" = "alpha" ]; then
    pass "T2: single matching entry → found, prints rig name"
else
    fail "T2: single matching entry → expected rc=0 out=alpha; got rc=$rc out=$out"
fi

# ── T3: one entry, non-matching db → not found ────────────────────────────
result=$(run_lookup $'rig_alpha_db|rig:alpha\n' "rig_other_db")
rc=$(printf '%s' "$result" | cut -d'|' -f1)
out=$(printf '%s' "$result" | cut -d'|' -f2-)
if [ "$rc" -ne 0 ] && [ -z "$out" ]; then
    pass "T3: no matching entry → not found"
else
    fail "T3: no matching entry → expected rc!=0 and empty output; got rc=$rc out=$out"
fi

# ── T4: multiple entries, second matches → found, correct rig name ───────
result=$(run_lookup $'rig_alpha_db|rig:alpha\nrig_beta_db|rig:beta\n' "rig_beta_db")
rc=$(printf '%s' "$result" | cut -d'|' -f1)
out=$(printf '%s' "$result" | cut -d'|' -f2-)
if [ "$rc" -eq 0 ] && [ "$out" = "beta" ]; then
    pass "T4: multiple entries, second matches → found, prints beta"
else
    fail "T4: multiple entries, second matches → expected rc=0 out=beta; got rc=$rc out=$out"
fi

# ── T5: matching entry but store_ref is not rig:-prefixed → not found ────
result=$(run_lookup $'city_db|city:main\n' "city_db")
rc=$(printf '%s' "$result" | cut -d'|' -f1)
out=$(printf '%s' "$result" | cut -d'|' -f2-)
if [ "$rc" -ne 0 ] && [ -z "$out" ]; then
    pass "T5: non-rig: store_ref → not found (defensive)"
else
    fail "T5: non-rig: store_ref → expected rc!=0 and empty output; got rc=$rc out=$out"
fi

# ── T6: db is a substring of another entry's db → exact match only ───────
result=$(run_lookup $'rig_alpha_db_extended|rig:alpha\n' "rig_alpha_db")
rc=$(printf '%s' "$result" | cut -d'|' -f1)
out=$(printf '%s' "$result" | cut -d'|' -f2-)
if [ "$rc" -ne 0 ] && [ -z "$out" ]; then
    pass "T6: substring db does not accidentally match → not found"
else
    fail "T6: substring db does not accidentally match → expected rc!=0 and empty output; got rc=$rc out=$out"
fi

[ "$FAILED" -eq 0 ] && exit 0 || exit 1
