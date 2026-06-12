#!/usr/bin/env bash
# test-slice.sh — auto-enrollment of test entrypoints into the
# host-provisioned gascity-test.slice systemd user slice.
#
# Hosts that provision a user-level gascity-test.slice (resource limits for
# test workloads) get every test entrypoint re-exec'd inside that slice via
# `systemd-run --user --scope`. Hosts without systemd-run, without a
# responsive user manager, or without the slice unit (CI runners, macOS,
# containers) run the entrypoint unchanged. Set GC_TEST_NO_SLICE=1 to opt
# out explicitly.
#
# Usage (from a bash entrypoint, with its absolute path resolved):
#   source "$repo_root/scripts/lib/test-slice.sh"
#   gc_test_slice_reexec "$repo_root/scripts/<entrypoint>" "$@"
#
# Self-test: scripts/test-slice-enroll-test (run by go test ./scripts).

GC_TEST_SLICE_UNIT="gascity-test.slice"

# Test seam: the self-test overrides the cgroup membership probe with a
# fixture file because /proc/self/cgroup cannot be faked.
: "${GC_TEST_SLICE_CGROUP_FILE:=/proc/self/cgroup}"

# gc_test_slice_should_wrap returns 0 when the calling entrypoint should
# re-exec inside gascity-test.slice, and 1 for plain execution.
gc_test_slice_should_wrap() {
  # Explicit opt-out: never touch systemd.
  [[ "${GC_TEST_NO_SLICE:-0}" != "1" ]] || return 1
  # Re-exec guard: the wrapped child runs plain even if the cgroup probe
  # below were to misfire, so enrollment can never loop.
  [[ "${GC_TEST_SLICE_ENROLLED:-0}" != "1" ]] || return 1
  command -v systemd-run >/dev/null 2>&1 || return 1
  command -v systemctl >/dev/null 2>&1 || return 1
  # Already inside the slice: nested runners (test-local-parallel invoking
  # the shard scripts) skip wrapping. cgroup membership survives the env -i
  # scrubbing those runners apply, unlike an env-var guard.
  if grep -qsF "$GC_TEST_SLICE_UNIT" "$GC_TEST_SLICE_CGROUP_FILE"; then
    return 1
  fi
  # One probe covers both "the user manager responds" and "the slice unit
  # exists": systemctl fails when the manager is unreachable and prints no
  # matching row when the unit file is absent. Capture instead of piping so
  # pipefail callers cannot misread an early grep exit as a failure.
  local unit_files
  unit_files="$(systemctl --user list-unit-files "$GC_TEST_SLICE_UNIT" 2>/dev/null)" || return 1
  printf '%s\n' "$unit_files" | grep -qsF "$GC_TEST_SLICE_UNIT" || return 1
  # Pre-flight: prove scope allocation actually works before committing the
  # real command to it (containers and stale sessions can pass the checks
  # above yet fail systemd-run). Fall back to plain execution otherwise.
  systemd-run --user --slice="$GC_TEST_SLICE_UNIT" --scope --collect --quiet \
    -- true >/dev/null 2>&1 || return 1
}

# gc_test_slice_reexec re-execs the given command line inside
# gascity-test.slice when available, and returns 0 (plain execution)
# otherwise. The first argument must be the absolute path of the calling
# entrypoint; the remaining arguments are its original argv.
gc_test_slice_reexec() {
  if gc_test_slice_should_wrap; then
    GC_TEST_SLICE_ENROLLED=1 exec systemd-run --user \
      --slice="$GC_TEST_SLICE_UNIT" --scope --collect --quiet -- "$@"
  fi
  return 0
}
