#!/bin/sh
# loadavg.sh — system load-average probe for the dolt-health watchdog.
# Sourced by mol-dog-doctor.sh; unit-tested by test/dolt/loadavg_test.sh.
#
# When the host is CPU-saturated the managed dolt sql-server starves: query
# latency climbs, connections pile up, and the client-side circuit breaker
# ("dolt circuit breaker is open") trips. A sustained system load average is an
# early, correlating signal of that destabilization.
#
# Reading the load average is a *local* probe — no dolt query, no connection —
# so it adds no load to the very server the watchdog is protecting. That is the
# watchdog's "SOFT" contract: never hammer dolt, and keep working when dolt is
# too wedged to answer.
#
# POSIX sh has no floating point, so loads like "2.34" are carried as integer
# hundredths ("centi", 234) and all comparisons are integer.

# cpu_count — echo the online CPU count (always >= 1). Cascade: explicit
# GC_DOCTOR_CPU_COUNT override (tests / cgroup-limited hosts), nproc (GNU
# coreutils), sysctl hw.ncpu (BSD/macOS), getconf, then 1. Always succeeds so a
# caller under `set -e` never aborts on it.
cpu_count() {
  if [ -n "${GC_DOCTOR_CPU_COUNT:-}" ]; then
    case "$GC_DOCTOR_CPU_COUNT" in
      ''|*[!0-9]*) : ;;
      *) if [ "$GC_DOCTOR_CPU_COUNT" -ge 1 ]; then
           printf '%s\n' "$GC_DOCTOR_CPU_COUNT"; return 0
         fi ;;
    esac
  fi
  _cc=$(nproc 2>/dev/null) || _cc=""
  case "$_cc" in ''|*[!0-9]*) _cc="" ;; esac
  if [ -z "$_cc" ]; then
    _cc=$(sysctl -n hw.ncpu 2>/dev/null) || _cc=""
    case "$_cc" in ''|*[!0-9]*) _cc="" ;; esac
  fi
  if [ -z "$_cc" ]; then
    _cc=$(getconf _NPROCESSORS_ONLN 2>/dev/null) || _cc=""
    case "$_cc" in ''|*[!0-9]*) _cc="" ;; esac
  fi
  if [ -z "$_cc" ] || [ "$_cc" -lt 1 ]; then _cc=1; fi
  printf '%s\n' "$_cc"
}

# loadavg_1min — echo the 1-minute load average as a decimal string. Override
# with GC_DOCTOR_LOADAVG_1MIN (tests / constrained hosts). Cascade:
# /proc/loadavg (Linux), sysctl -n vm.loadavg (BSD/macOS), uptime parse. Always
# succeeds — echoes 0 when no source is available, so the caller's arithmetic
# never sees an empty string.
loadavg_1min() {
  if [ -n "${GC_DOCTOR_LOADAVG_1MIN:-}" ]; then
    printf '%s\n' "$GC_DOCTOR_LOADAVG_1MIN"; return 0
  fi
  if [ -r /proc/loadavg ]; then
    _la=$(cut -d' ' -f1 /proc/loadavg 2>/dev/null) || _la=""
    case "$_la" in *[0-9]*) printf '%s\n' "$_la"; return 0 ;; esac
  fi
  # vm.loadavg prints "{ 1.23 4.56 7.89 }"; take the first number.
  _la=$(sysctl -n vm.loadavg 2>/dev/null) || _la=""
  case "$_la" in
    *[0-9]*)
      _la=$(printf '%s\n' "$_la" | tr -d '{}' | awk '{print $1}')
      case "$_la" in *[0-9]*) printf '%s\n' "$_la"; return 0 ;; esac
      ;;
  esac
  _la=$(uptime 2>/dev/null | sed -n 's/.*load average[s]*:[[:space:]]*\([0-9][0-9.]*\).*/\1/p') || _la=""
  case "$_la" in *[0-9]*) printf '%s\n' "$_la"; return 0 ;; esac
  printf '0\n'
}

# to_centi DECIMAL — convert a non-negative decimal like "2.34" or "10" to
# integer hundredths ("234", "1000") for integer comparison. Truncates beyond
# two decimals; clamps anything non-numeric to 0. Always succeeds.
to_centi() {
  _v="${1:-0}"
  case "$_v" in ''|*[!0-9.]*) printf '0\n'; return 0 ;; esac
  _int=${_v%%.*}
  case "$_v" in *.*) _frac=${_v#*.} ;; *) _frac="" ;; esac
  case "$_int" in ''|*[!0-9]*) _int=0 ;; esac
  # Pad/truncate the fraction to exactly two digits.
  _frac=$(printf '%s' "${_frac}00" | cut -c1-2)
  case "$_frac" in ''|*[!0-9]*) _frac=0 ;; esac
  printf '%s\n' "$(( _int * 100 + _frac ))"
}

# centi_to_dec CENTI — inverse of to_centi for display, e.g. 234 -> "2.34".
centi_to_dec() {
  _c="${1:-0}"
  case "$_c" in ''|*[!0-9]*) _c=0 ;; esac
  printf '%d.%02d\n' "$(( _c / 100 ))" "$(( _c % 100 ))"
}

# loadavg_should_warn LOAD_CENTI CPUS THRESHOLD_CENTI_PER_CORE — exit 0 (warn)
# when per-core load (LOAD_CENTI / CPUS) meets or exceeds the per-core
# threshold. Compared as LOAD_CENTI >= CPUS * THRESHOLD to keep integer math,
# e.g. a 4.00/core threshold on 8 cores warns at load >= 32.00. A threshold of
# 0 (or non-numeric) disables the check.
loadavg_should_warn() {
  _lc="${1:-0}"; _cpus="${2:-1}"; _thc="${3:-0}"
  case "$_lc" in ''|*[!0-9]*) _lc=0 ;; esac
  case "$_cpus" in ''|*[!0-9]*) _cpus=1 ;; esac
  [ "$_cpus" -ge 1 ] || _cpus=1
  case "$_thc" in ''|*[!0-9]*) _thc=0 ;; esac
  [ "$_thc" -gt 0 ] || return 1
  [ "$_lc" -ge "$(( _cpus * _thc ))" ]
}
