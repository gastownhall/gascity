#!/bin/sh
# Unit test for examples/dolt/assets/scripts/latency.sh.
#
# Proves the fix for the whole-second latency bug in mol-dog-doctor.sh:
# (a) now_ms has sub-second resolution, (b) the warn decision does not
# false-trip on a fast (sub-second) probe but still fires on real slowness.
#
# Run: sh test/dolt/latency_test.sh
set -u
HERE=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
LATENCY_LIB="${LATENCY_LIB:-$HERE/../../examples/dolt/assets/scripts/latency.sh}"

if [ ! -f "$LATENCY_LIB" ]; then
  echo "FAIL: latency helper not found at $LATENCY_LIB"
  exit 1
fi
# shellcheck disable=SC1090
. "$LATENCY_LIB"

fail=0
pass() { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

command -v now_ms >/dev/null 2>&1 || { echo "FAIL: now_ms not defined"; exit 1; }
command -v latency_should_warn >/dev/null 2>&1 || { echo "FAIL: latency_should_warn not defined"; exit 1; }

# now_ms has sub-second resolution. A 50ms sleep must measure in a sub-second
# band; a whole-second clock yields 0 or 1000 — both fail this.
s=$(now_ms); sleep 0.05; e=$(now_ms); d=$((e - s))
if [ "$d" -ge 5 ] && [ "$d" -le 800 ]; then
  pass "now_ms sub-second resolution (${d}ms for a 50ms sleep)"
else
  bad "now_ms resolution: got ${d}ms, want 5..800 (whole-second clock yields 0 or 1000)"
fi

# A fast probe does NOT warn at the default 1000ms threshold.
if latency_should_warn 50 1000; then bad "50ms probe warned at 1000ms threshold (false positive)"; else pass "50ms probe -> no warn"; fi

# A genuinely slow probe warns.
if latency_should_warn 1500 1000; then pass "1500ms probe -> warn"; else bad "1500ms probe did not warn"; fi

# Boundary equality warns (>= semantics preserved).
if latency_should_warn 1000 1000; then pass "1000ms == threshold -> warn"; else bad "boundary 1000ms did not warn"; fi

# Regression of the original bug — 30 fast probes must NEVER false-warn.
i=0; warned=0
while [ "$i" -lt 30 ]; do
  s=$(now_ms); sleep 0.02; e=$(now_ms)
  if latency_should_warn $((e - s)) 1000; then warned=$((warned + 1)); fi
  i=$((i + 1))
done
if [ "$warned" -eq 0 ]; then pass "30 fast probes -> 0 false warns"; else bad "$warned/30 fast probes false-warned"; fi

echo "----"
if [ "$fail" -eq 0 ]; then echo "ALL PASS"; else echo "FAILURES PRESENT"; fi
exit "$fail"
