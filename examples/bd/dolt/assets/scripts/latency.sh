#!/bin/sh
# latency.sh — millisecond-resolution latency measurement for dolt-pack
# health probes. Sourced by mol-dog-doctor.sh; unit-tested by
# test/dolt/latency_test.sh.
#
# Replaces whole-second 'date +%s' timing, which quantizes a sub-second probe
# to 0s or 1s depending on whether it straddles a wall-clock second tick —
# producing false latency WARNs (and MEDIUM advisory mail) at a 1s threshold.

# _now_ms_plausible VALUE — exit 0 when VALUE looks like an epoch-millisecond
# reading: all digits, 13 or 14 of them (epoch-ms is 13 digits from 2001-09-09
# through 2286-11-20, 14 thereafter). The upper bound rejects a 19-digit
# epoch-NANOSECOND reading, which a 'date +%s%3N' that ignores the %3 width
# (some coreutils builds, e.g. WSL2) emits — otherwise it would pass as ms and
# inflate latency ~1e6×, falsely tripping the advisory threshold. Over-long
# readings fall through to the perl/python3 fallbacks, which return real ms.
_now_ms_plausible() {
  case "${1:-}" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "${#1}" -ge 13 ] && [ "${#1}" -le 14 ]
}

# now_ms — echo the current time in epoch milliseconds.
#
# Implementation cascade; the first plausible reading wins:
#   1. date +%s%3N      — GNU/coreutils date (%3N = milliseconds). BSD/macOS
#                         date has no %N and prints a literal '3N' suffix,
#                         which the plausibility check rejects.
#   2. perl Time::HiRes — core module since perl 5.8; present on stock macOS
#                         and virtually every Linux.
#   3. python3          — time.time() carries sub-millisecond resolution.
#   4. date +%s × 1000  — whole seconds; no worse than the pre-fix behavior.
#
# The cascade exists because a GNU-only implementation silently degrades to
# whole seconds on BSD/macOS, where a sub-second probe that straddles a
# wall-clock second tick measures 1000ms and false-trips the default 1000ms
# warn threshold — the same advisory storm the millisecond rewrite was meant
# to stop, in different units.
now_ms() {
  _now_ms_v=$(date +%s%3N 2>/dev/null)
  if _now_ms_plausible "$_now_ms_v"; then
    printf '%s\n' "$_now_ms_v"
    return 0
  fi
  _now_ms_v=$(perl -MTime::HiRes=time -e 'printf "%.0f\n", time() * 1000' 2>/dev/null)
  if _now_ms_plausible "$_now_ms_v"; then
    printf '%s\n' "$_now_ms_v"
    return 0
  fi
  _now_ms_v=$(python3 -c 'import time; print(int(time.time() * 1000))' 2>/dev/null)
  if _now_ms_plausible "$_now_ms_v"; then
    printf '%s\n' "$_now_ms_v"
    return 0
  fi
  printf '%s\n' "$(( $(date +%s) * 1000 ))"
}

# latency_should_warn ELAPSED_MS THRESHOLD_MS — exit 0 (warn) when measured
# latency meets or exceeds the threshold, 1 otherwise. Preserves the original
# '>=' semantics, now in milliseconds.
latency_should_warn() {
  [ "${1:-0}" -ge "${2:-0}" ]
}
