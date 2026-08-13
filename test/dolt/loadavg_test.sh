#!/bin/sh
# Unit test for examples/bd/dolt/assets/scripts/loadavg.sh.
#
# The dolt-health watchdog reads the host load average as a *local*, dolt-free
# early-warning signal that the managed server is about to starve under CPU
# pressure. This proves the integer-hundredths ("centi") math, the per-core warn
# decision (including the disable and boundary cases), the source overrides used
# by the doctor's tests, and — critically — that every helper is safe under the
# caller's `set -e` (it must never abort the doctor).
#
# Run: sh test/dolt/loadavg_test.sh
set -u
HERE=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
LOADAVG_LIB="${LOADAVG_LIB:-$HERE/../../examples/bd/dolt/assets/scripts/loadavg.sh}"

if [ ! -f "$LOADAVG_LIB" ]; then
  echo "FAIL: loadavg helper not found at $LOADAVG_LIB"
  exit 1
fi
# shellcheck disable=SC1090
. "$LOADAVG_LIB"

fail=0
pass() { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

for fn in cpu_count loadavg_1min to_centi centi_to_dec loadavg_should_warn; do
  command -v "$fn" >/dev/null 2>&1 || { echo "FAIL: $fn not defined"; exit 1; }
done

# eq NAME GOT WANT — assert string equality.
eq() {
  if [ "$2" = "$3" ]; then pass "$1 -> $2"; else bad "$1: got '$2', want '$3'"; fi
}

# --- to_centi: decimal -> integer hundredths -------------------------------
eq "to_centi 2.34"  "$(to_centi 2.34)"  "234"
eq "to_centi 10"    "$(to_centi 10)"    "1000"
eq "to_centi 0.5"   "$(to_centi 0.5)"   "50"
eq "to_centi 0.05"  "$(to_centi 0.05)"  "5"
eq "to_centi 0"     "$(to_centi 0)"     "0"
eq "to_centi 3.999(trunc)" "$(to_centi 3.999)" "399"
eq "to_centi abc(clamp)"   "$(to_centi abc)"   "0"
eq "to_centi ''(clamp)"    "$(to_centi '')"    "0"

# --- centi_to_dec: integer hundredths -> decimal ---------------------------
eq "centi_to_dec 234"  "$(centi_to_dec 234)"  "2.34"
eq "centi_to_dec 1000" "$(centi_to_dec 1000)" "10.00"
eq "centi_to_dec 5"    "$(centi_to_dec 5)"    "0.05"
eq "centi_to_dec 0"    "$(centi_to_dec 0)"    "0.00"

# Round-trip: a value formatted then re-parsed is unchanged.
for v in 234 1000 5 4 3200; do
  rt=$(to_centi "$(centi_to_dec "$v")")
  eq "round-trip $v" "$rt" "$v"
done

# --- loadavg_should_warn: per-core threshold, integer compare --------------
# 32.00 over 8 cores == 4.00/core, boundary of a 4.00/core threshold -> warn.
if loadavg_should_warn 3200 8 400; then pass "32.0/8core >= 4.0/core -> warn"; else bad "boundary 4.0/core did not warn"; fi
if loadavg_should_warn 3199 8 400; then bad "31.99/8core warned below 4.0/core"; else pass "31.99/8core -> no warn"; fi
# Single core boundary.
if loadavg_should_warn 400 1 400; then pass "4.0/1core == threshold -> warn"; else bad "single-core boundary did not warn"; fi
if loadavg_should_warn 399 1 400; then bad "3.99/1core warned"; else pass "3.99/1core -> no warn"; fi
# Threshold 0 (or non-numeric) disables the check entirely.
if loadavg_should_warn 999999 1 0; then bad "threshold 0 did not disable the check"; else pass "threshold 0 disables"; fi
if loadavg_should_warn 999999 1 abc; then bad "non-numeric threshold did not disable"; else pass "non-numeric threshold disables"; fi
# A zero CPU count is coerced to 1, not a divide-by-zero.
if loadavg_should_warn 500 0 400; then pass "cpu=0 coerced to 1 (5.0 >= 4.0 -> warn)"; else bad "cpu=0 not coerced to 1"; fi

# --- source overrides (used by the doctor's Go tests) ----------------------
eq "cpu_count override 4"       "$(GC_DOCTOR_CPU_COUNT=4 cpu_count)"        "4"
eq "loadavg_1min override 7.5"  "$(GC_DOCTOR_LOADAVG_1MIN=7.5 loadavg_1min)" "7.5"
# An invalid CPU override falls back to a real count (always >= 1).
oc=$(GC_DOCTOR_CPU_COUNT=0 cpu_count)
if [ "$oc" -ge 1 ] 2>/dev/null; then pass "cpu_count invalid override falls back to >=1 ($oc)"; else bad "cpu_count invalid override: '$oc'"; fi

# --- live sources are numeric and never empty ------------------------------
rc=$(cpu_count)
if [ "$rc" -ge 1 ] 2>/dev/null; then pass "cpu_count (live) >= 1 ($rc)"; else bad "cpu_count (live) not a positive integer: '$rc'"; fi
rl=$(loadavg_1min)
case "$rl" in *[0-9]*) pass "loadavg_1min (live) is numeric ($rl)" ;; *) bad "loadavg_1min (live) non-numeric: '$rl'" ;; esac

# --- SOFT contract: never abort the caller under set -e --------------------
# Each helper is only ever evaluated inside the doctor's `if`/command-subst;
# under `set -e` a non-zero return from a bare call would abort. Prove the
# no-signal paths (empty/garbage input) still exit 0 when called bare.
if ( set -e; . "$LOADAVG_LIB"; cpu_count >/dev/null; loadavg_1min >/dev/null; to_centi '' >/dev/null; centi_to_dec '' >/dev/null; true ); then
  pass "helpers do not abort under set -e"
else
  bad "a helper aborted a set -e caller"
fi

echo "----"
if [ "$fail" -eq 0 ]; then echo "ALL PASS"; else echo "FAILURES PRESENT"; fi
exit "$fail"
