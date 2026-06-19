#!/bin/sh
# Unit test for examples/bd/dolt/assets/scripts/latency.sh.
#
# Proves the fix for the whole-second latency bug in mol-dog-doctor.sh:
# (a) now_ms has sub-second resolution, (b) the warn decision does not
# false-trip on a fast (sub-second) probe but still fires on real slowness.
#
# Run: sh test/dolt/latency_test.sh
set -u
HERE=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
LATENCY_LIB="${LATENCY_LIB:-$HERE/../../examples/bd/dolt/assets/scripts/latency.sh}"

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

# --- Fallback-cascade coverage ---------------------------------------------
# A PATH shim emulates BSD/macOS date: '+%s%3N' prints a literal '3N' suffix
# (no %N support); every other format defers to the real date. This forces
# now_ms off the GNU-date branch on any platform, so the perl/python3
# fallbacks are exercised even on GNU/Linux CI.

SHIM_DIR=$(mktemp -d)
trap 'rm -rf "$SHIM_DIR"' EXIT
REAL_DATE=$(command -v date)

mkdir -p "$SHIM_DIR/bsd"
cat > "$SHIM_DIR/bsd/date" <<EOF
#!/bin/sh
if [ "\${1:-}" = "+%s%3N" ]; then
  printf '%s3N\n' "\$("$REAL_DATE" +%s)"
else
  exec "$REAL_DATE" "\$@"
fi
EOF
chmod +x "$SHIM_DIR/bsd/date"

# Without GNU date, a sub-second clock must still come from perl or python3.
if command -v perl >/dev/null 2>&1 || command -v python3 >/dev/null 2>&1; then
  d=$( (
    PATH="$SHIM_DIR/bsd:$PATH"; export PATH
    s=$(now_ms); sleep 0.05; e=$(now_ms); echo $((e - s))
  ) )
  if [ "$d" -ge 5 ] && [ "$d" -le 800 ]; then
    pass "BSD-date shim: perl/python3 fallback keeps sub-second resolution (${d}ms)"
  else
    bad "BSD-date shim: got ${d}ms for a 50ms sleep, want 5..800 (fallback degraded to whole seconds)"
  fi
else
  echo "SKIP: neither perl nor python3 on PATH; cannot exercise sub-second fallbacks"
fi

# A PATH shim emulates a coreutils 'date' that ignores the %3 width in
# '+%s%3N' (observed: WSL2): %N emits all 9 nanosecond digits, yielding a
# 19-digit all-digit value. The lower-bound-only plausibility guard accepted
# it as epoch-ms (~1e6x inflated latency); the upper bound now rejects it, so
# the perl/python3 fallback must still produce a sub-second reading.
mkdir -p "$SHIM_DIR/wsl"
cat > "$SHIM_DIR/wsl/date" <<EOF
#!/bin/sh
if [ "\${1:-}" = "+%s%3N" ]; then
  printf '%s%09d\n' "\$("$REAL_DATE" +%s)" 0
else
  exec "$REAL_DATE" "\$@"
fi
EOF
chmod +x "$SHIM_DIR/wsl/date"

if command -v perl >/dev/null 2>&1 || command -v python3 >/dev/null 2>&1; then
  d=$( (
    PATH="$SHIM_DIR/wsl:$PATH"; export PATH
    s=$(now_ms); sleep 0.05; e=$(now_ms); echo $((e - s))
  ) )
  if [ "$d" -ge 5 ] && [ "$d" -le 800 ]; then
    pass "WSL-date shim: 19-digit ns rejected, perl/python3 fallback keeps sub-second resolution (${d}ms)"
  else
    bad "WSL-date shim: got ${d}ms for a 50ms sleep, want 5..800 (19-digit ns accepted as ms)"
  fi
else
  echo "SKIP: neither perl nor python3 on PATH; cannot exercise sub-second fallbacks"
fi

# With GNU date, perl, and python3 all unavailable, now_ms must degrade to
# whole seconds (a plausible epoch reading ending in 000) rather than emit
# garbage or crash.
mkdir -p "$SHIM_DIR/bare"
cp "$SHIM_DIR/bsd/date" "$SHIM_DIR/bare/date"
for tool in perl python3; do
  printf '#!/bin/sh\nexit 127\n' > "$SHIM_DIR/bare/$tool"
  chmod +x "$SHIM_DIR/bare/$tool"
done
v=$( (PATH="$SHIM_DIR/bare:$PATH"; export PATH; now_ms) )
case "$v" in
  *[!0-9]*|'')
    bad "exhausted-cascade now_ms emitted non-numeric output: '$v'"
    ;;
  *000)
    if [ "${#v}" -ge 13 ]; then
      pass "exhausted cascade degrades to whole-second epoch-ms ($v)"
    else
      bad "exhausted-cascade now_ms too short for epoch-ms: '$v'"
    fi
    ;;
  *)
    bad "exhausted-cascade now_ms not whole-second-quantized: '$v'"
    ;;
esac

echo "----"
if [ "$fail" -eq 0 ]; then echo "ALL PASS"; else echo "FAILURES PRESENT"; fi
exit "$fail"
