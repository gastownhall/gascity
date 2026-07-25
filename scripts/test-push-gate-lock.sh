#!/usr/bin/env bash
#
# test-push-gate-lock.sh — unit tests for the pre-push gate's cross-invocation
# concurrency bound (ga-owh20p) plus static assertions that it's wired into
# scripts/test-local-parallel correctly.
#
# The slot mechanics live in scripts/push-gate-lock-lib.sh and are exercised
# directly here — no real city, no real test suite run. Cross-process denial
# is tested deterministically with flock(1) probes, so there is no reliance
# on timing EXCEPT the one case that inherently requires wall-clock: the
# wait-then-timeout path, which uses second-scale overrides
# (PUSH_GATE_MAX_WAIT_SECONDS / PUSH_GATE_POLL_SECONDS) to stay fast.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="$TEST_DIR/push-gate-lock-lib.sh"
LOCAL_PARALLEL="$TEST_DIR/test-local-parallel"

# shellcheck source=./push-gate-lock-lib.sh disable=SC1091
. "$LIB"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }

assert_eq() {
    local name="$1" got="$2" want="$3"
    if [[ "$got" == "$want" ]]; then record_pass "$name"
    else record_fail "$name" "got '$got', want '$want'"; fi
}
assert_true()  { if "${@:2}"; then record_pass "$1"; else record_fail "$1" "expected true"; fi; }
assert_false() { if "${@:2}"; then record_fail "$1" "expected false"; else record_pass "$1"; fi; }
assert_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" == *"$needle"* ]]; then record_pass "$name"
    else record_fail "$name" "missing '$needle' in: $haystack"; fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
SLOTS="$WORK/gate-slots"

# ---------------- acquire / hold (two slots) ----------------
export PUSH_GATE_MAX_CONCURRENT=2
FD0=""
if push_gate_acquire_slot "$SLOTS" FD0 "holder-A"; then
    record_pass "acquire.first_succeeds"
else
    record_fail "acquire.first_succeeds" "expected return 0"
fi
assert_true "acquire.fd_var_set" test -n "$FD0"

FD1=""
if push_gate_acquire_slot "$SLOTS" FD1 "holder-B"; then
    record_pass "acquire.second_succeeds_distinct_slot"
else
    record_fail "acquire.second_succeeds_distinct_slot" "expected return 0 (max=2)"
fi
assert_true "acquire.second_fd_differs" test "$FD0" != "$FD1"

HOLDERS="$(push_gate_describe_slots "$SLOTS" 2)"
assert_contains "describe.has_pid_a"   "$HOLDERS" "$$"
assert_contains "describe.has_label_a" "$HOLDERS" "holder-A"
assert_contains "describe.has_label_b" "$HOLDERS" "holder-B"

# ---------------- denial while both slots held (cross-process, deterministic) ----------------
if flock -n "$SLOTS/slot-0.lock" -c 'exit 0'; then
    record_fail "deny.flock_probe_slot0" "child acquired a slot we hold"
else
    record_pass "deny.flock_probe_slot0"
fi
if flock -n "$SLOTS/slot-1.lock" -c 'exit 0'; then
    record_fail "deny.flock_probe_slot1" "child acquired a slot we hold"
else
    record_pass "deny.flock_probe_slot1"
fi
CHILD_HELD="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=0 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z holder-C; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "deny.lib_from_child_zero_wait" "$CHILD_HELD" "DENIED"

# ---------------- all slots busy: waits, prints diagnostics, then times out with rc 1 ----------------
START="$(date +%s)"
CHILD_OUT="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=2 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z holder-D; then echo ACQUIRED; else echo "DENIED rc=$?"; fi' 2>&1)"
CHILD_RC=$?
ELAPSED=$(( $(date +%s) - START ))
assert_contains "wait.busy_message_immediate" "$CHILD_OUT" "busy"
assert_contains "wait.reports_holder_a"        "$CHILD_OUT" "holder-A"
assert_contains "wait.timeout_message"         "$CHILD_OUT" "timed out"
assert_contains "wait.eventually_denied"       "$CHILD_OUT" "DENIED"
assert_true     "wait.elapsed_at_least_bound"  test "$ELAPSED" -ge 2
assert_eq       "wait.subshell_exits_zero"     "$CHILD_RC" "0"

# ---------------- release -> reacquire ----------------
push_gate_release_slot "$FD0"
if flock -n "$SLOTS/slot-0.lock" -c 'exit 0'; then
    record_pass "release.flock_probe_succeeds"
else
    record_fail "release.flock_probe_succeeds" "could not acquire slot-0 after release"
fi
CHILD_FREE="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=1 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z holder-E; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "release.lib_from_child" "$CHILD_FREE" "ACQUIRED"
push_gate_release_slot "$FD1"

# ---------------- escape hatch: GC_PUSH_GATE_NO_CAP bypasses the cap entirely ----------------
NOCAP_OUT="$(LIB="$LIB" DIR="$SLOTS" GC_PUSH_GATE_NO_CAP=1 PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=0 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "no_cap.bypasses_full_slots" "$NOCAP_OUT" "ACQUIRED"

# ---------------- city-root resolution: env short-circuit ----------------
CITY_ENV="$WORK/city-env"
mkdir -p "$CITY_ENV"
: >"$CITY_ENV/city.toml"
RESOLVED_ENV="$(GC_CITY_PATH="$CITY_ENV" GC_CITY="" GC_CITY_ROOT="" push_gate_city_root)"
assert_eq "city_root.env_short_circuit" "$RESOLVED_ENV" "$CITY_ENV"

# An env var pointing at a directory with neither city.toml nor .gc/ must NOT
# be trusted verbatim — it should fall through to walk-up instead of
# returning garbage. Bounded entirely inside $WORK (HOME=$WORK as the
# ceiling) so this can't accidentally pick up a real city.toml somewhere in
# the ambient filesystem's actual ancestry.
BOGUS="$WORK/not-a-city"
mkdir -p "$BOGUS/nested"
if RESOLVED_BOGUS="$(cd "$BOGUS/nested" && GC_CITY_PATH="$BOGUS" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK" push_gate_city_root)"; then
    assert_true "city_root.rejects_unvalidated_env" test "$RESOLVED_BOGUS" != "$BOGUS"
else
    record_pass "city_root.rejects_unvalidated_env"
fi

# ---------------- city-root resolution: walk-up discovery ----------------
CITY_WALK="$WORK/city-walk"
mkdir -p "$CITY_WALK/rigs/proj/sub"
: >"$CITY_WALK/city.toml"
RESOLVED_WALK="$(cd "$CITY_WALK/rigs/proj/sub" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_city_root)"
assert_eq "city_root.walk_up_finds_ancestor" "$RESOLVED_WALK" "$CITY_WALK"

# ---------------- slots dir: derives from city root, falls back to repo-relative ----------------
SLOTS_FROM_CITY="$(GC_CITY_PATH="$CITY_ENV" GC_CITY="" GC_CITY_ROOT="" push_gate_slots_dir)"
assert_eq "slots_dir.under_city_root" "$SLOTS_FROM_CITY" "$CITY_ENV/.gc/gate-slots"

NOCITY="$WORK/no-city-repo"
mkdir -p "$NOCITY"
(cd "$NOCITY" && git init -q .) 2>/dev/null || true
SLOTS_FALLBACK="$(cd "$NOCITY" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_slots_dir)"
assert_eq "slots_dir.falls_back_to_repo_relative" "$SLOTS_FALLBACK" "$NOCITY/.git/gate-slots"

# ---------------- static wiring assertions against test-local-parallel ----------------
assert_true "wiring.sources_lib"   grep -q 'push-gate-lock-lib.sh' "$LOCAL_PARALLEL"
assert_true "wiring.calls_acquire" grep -q 'push_gate_acquire_slot' "$LOCAL_PARALLEL"
assert_true "wiring.has_override"  grep -q 'GC_PUSH_GATE_NO_CAP'   "$LOCAL_PARALLEL"

acq_line="$(grep -n 'push_gate_acquire_slot ' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
if [[ -n "$acq_line" ]]; then
    LOSER_BLOCK="$(sed -n "${acq_line},$((acq_line + 10))p" "$LOCAL_PARALLEL")"
    assert_contains "wiring.timeout_exits_75" "$LOSER_BLOCK" "exit 75"
else
    record_fail "wiring.timeout_exits_75" "no push_gate_acquire_slot call found in $LOCAL_PARALLEL"
fi
# Must be the release call wired to an EXIT trap specifically — not just any
# trap and any release call existing independently somewhere in the file
# (e.g. an unrelated per-job cleanup trap for a temp dir).
assert_true "wiring.releases_slot_on_exit" grep -qE 'trap .*push_gate_release_slot.*EXIT' "$LOCAL_PARALLEL"

echo
echo "push-gate-lock tests: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
