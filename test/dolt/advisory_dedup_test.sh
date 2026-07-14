#!/bin/sh
# Unit test for examples/bd/dolt/assets/scripts/advisory_state.sh.
#
# Proves the dedup fix for the dolt-health advisory storm in mol-dog-doctor.sh
# (#3409): a persistent condition sends exactly one advisory (not one per tick),
# a changed condition set re-alerts, and a healthy server clears the state so the
# next occurrence alerts again.
#
# Run: sh test/dolt/advisory_dedup_test.sh
set -u
HERE=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
ADVISORY_LIB="${ADVISORY_LIB:-$HERE/../../examples/bd/dolt/assets/scripts/advisory_state.sh}"

if [ ! -f "$ADVISORY_LIB" ]; then
  echo "FAIL: advisory helper not found at $ADVISORY_LIB"
  exit 1
fi
# shellcheck disable=SC1090
. "$ADVISORY_LIB"

fail=0
pass() { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

command -v advisory_changed >/dev/null 2>&1 || { echo "FAIL: advisory_changed not defined"; exit 1; }
command -v advisory_record  >/dev/null 2>&1 || { echo "FAIL: advisory_record not defined"; exit 1; }
command -v advisory_clear   >/dev/null 2>&1 || { echo "FAIL: advisory_clear not defined"; exit 1; }

# Reap work dirs left by a run of this script that was SIGKILLed or timed
# out: SIGKILL cannot be trapped, so `trap ... EXIT` below never fires for
# a killed run. This sweep is the backstop, keying on the creator's PID
# embedded in the dir name (ga-ntbpyb.1 -- same fragility class as the tmux
# socket parent dir leak; smaller blast radius so no age guard is needed).
TMPROOT="${TMPDIR:-/tmp}"
for d in "$TMPROOT"/gc-dolt-advisory-test-work.*; do
  [ -d "$d" ] || continue
  base=${d##*/}
  rest=${base#gc-dolt-advisory-test-work.}
  pid=${rest%%.*}
  case "$pid" in
    ''|*[!0-9]*) continue ;;
  esac
  kill -0 "$pid" 2>/dev/null || rm -rf "$d"
done

WORK=$(mktemp -d "$TMPROOT/gc-dolt-advisory-test-work.$$.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
STATE="$WORK/doctor-advisory-state"

# No state recorded yet -> first advisory must send.
if advisory_changed "latency " "$STATE"; then pass "first advisory (no state) -> send"; else bad "first advisory suppressed with no state on file"; fi

# Recording persists the signature.
advisory_record "latency " "$STATE"
if [ -f "$STATE" ]; then pass "record creates the state file"; else bad "record did not create the state file"; fi

# Same signature after a record -> suppressed (the storm fix).
if advisory_changed "latency " "$STATE"; then bad "identical advisory re-sent (storm not deduped)"; else pass "identical advisory -> suppressed"; fi

# A simulated 50-tick run with an unchanged condition sends nothing further.
resends=0
i=0
while [ "$i" -lt 50 ]; do
  if advisory_changed "latency " "$STATE"; then resends=$((resends + 1)); advisory_record "latency " "$STATE"; fi
  i=$((i + 1))
done
if [ "$resends" -eq 0 ]; then pass "50 steady ticks -> 0 re-sends"; else bad "$resends/50 steady ticks re-sent"; fi

# A changed condition set -> re-alert.
if advisory_changed "latency conn " "$STATE"; then pass "changed condition set -> re-alert"; else bad "changed condition set did not re-alert"; fi
advisory_record "latency conn " "$STATE"

# Healthy server clears state; the next occurrence alerts again.
advisory_clear "$STATE"
if [ -f "$STATE" ]; then bad "clear did not remove the state file"; else pass "clear removes the state file"; fi
if advisory_changed "latency " "$STATE"; then pass "post-clear recurrence -> re-alert"; else bad "post-clear recurrence suppressed"; fi

# Fail-open: with no state-file path, never suppress (degrade to pre-fix alert,
# never to silence).
if advisory_changed "latency " ""; then pass "empty state path -> fail open (send)"; else bad "empty state path suppressed the advisory"; fi

# record into a not-yet-existing directory creates the path (mkdir -p).
NESTED="$WORK/runtime/packs/dolt/doctor-advisory-state"
advisory_record "orphan " "$NESTED"
if [ -f "$NESTED" ]; then pass "record creates missing parent directories"; else bad "record did not create nested state path"; fi

# The recorded signature round-trips exactly.
got=$(cat "$NESTED" 2>/dev/null || true)
if [ "$got" = "orphan " ]; then pass "recorded signature round-trips"; else bad "recorded signature mismatch: got '$got'"; fi

echo "----"
if [ "$fail" -eq 0 ]; then echo "ALL PASS"; else echo "FAILURES PRESENT"; fi
exit "$fail"
