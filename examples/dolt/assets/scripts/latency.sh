#!/bin/sh
# latency.sh — millisecond-resolution latency measurement for dolt-pack
# health probes. Sourced by mol-dog-doctor.sh; unit-tested by
# test/dolt/latency_test.sh.
#
# Replaces whole-second 'date +%s' timing, which quantizes a sub-second probe
# to 0s or 1s depending on whether it straddles a wall-clock second tick —
# producing false latency WARNs (and MEDIUM advisory mail) at a 1s threshold.

# now_ms — echo the current time in epoch milliseconds.
# GNU/coreutils date supports %3N (3-digit nanoseconds = milliseconds). On
# platforms whose date lacks %N (e.g. BSD/macOS without coreutils), fall back
# to second-resolution * 1000 — no worse than the prior whole-second behavior.
now_ms() {
  _now_ms_v=$(date +%s%3N 2>/dev/null)
  case "$_now_ms_v" in
    ''|*[!0-9]*) _now_ms_v="" ;;   # %3N printed literally: no ms support
  esac
  if [ -n "$_now_ms_v" ] && [ "${#_now_ms_v}" -ge 13 ]; then
    printf '%s\n' "$_now_ms_v"     # epoch-ms is 13+ digits from 2001..2286
  else
    printf '%s\n' "$(( $(date +%s) * 1000 ))"
  fi
}

# latency_should_warn ELAPSED_MS THRESHOLD_MS — exit 0 (warn) when measured
# latency meets or exceeds the threshold, 1 otherwise. Preserves the original
# '>=' semantics, now in milliseconds.
latency_should_warn() {
  [ "${1:-0}" -ge "${2:-0}" ]
}
