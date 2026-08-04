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
# socket parent dir leak).
#
# Only dirs older than the age guard are eligible. A directory's mtime is
# visible across PID namespaces, whereas `kill -0 <pid>` is not: when two
# runs share /tmp in distinct PID namespaces, a live sibling's host PID can
# read as dead, so a PID-only sweep could rm -rf its fresh WORK dir out from
# under it and flake it. The age guard mirrors test/tmuxtest's
# socketParentSweepMinAge (1h) so a just-created sibling is never eligible and
# only genuine orphans are swept. Among eligible dirs we still skip any whose
# creator PID is alive, treating EPERM as alive to match internal/pidutil.Alive
# rather than removing on it. The probe runs under LC_ALL=C so the EPERM message
# is the deterministic C-locale text the case below matches; strerror is
# otherwise localized and a non-C locale would reap a live foreign-user dir.
TMPROOT="${TMPDIR:-/tmp}"
find "$TMPROOT" -maxdepth 1 -type d -name 'gc-dolt-advisory-test-work.*' -mmin +60 2>/dev/null |
  while IFS= read -r d; do
    base=${d##*/}
    rest=${base#gc-dolt-advisory-test-work.}
    pid=${rest%%.*}
    case "$pid" in
      ''|*[!0-9]*) continue ;;
    esac
    if kill_err=$(LC_ALL=C kill -0 "$pid" 2>&1); then
      continue
    fi
    case "$kill_err" in
      *ermitted*|*ermission*) continue ;;
    esac
    rm -rf "$d"
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
