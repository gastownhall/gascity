#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCAL_PARALLEL="$REPO_ROOT/scripts/test-local-parallel"

pass=0
fail=0

record_pass() {
  printf 'ok - %s\n' "$1"
  pass=$((pass + 1))
}

record_fail() {
  printf 'not ok - %s: %s\n' "$1" "$2" >&2
  fail=$((fail + 1))
}

assert_eq() {
  local name="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then
    record_pass "$name"
  else
    record_fail "$name" "got '$got', want '$want'"
  fi
}

assert_contains() {
  local name="$1" haystack="$2" needle="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    record_pass "$name"
  else
    record_fail "$name" "missing '$needle'"
  fi
}

assert_not_contains() {
  local name="$1" haystack="$2" needle="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    record_fail "$name" "unexpected '$needle'"
  else
    record_pass "$name"
  fi
}

build_fixture_runner() {
  local fixture="$1" events_q="$2"

  mkdir -p "$fixture/scripts/lib"
  cp -f "$REPO_ROOT/scripts/lib/test-slice.sh" "$fixture/scripts/lib/test-slice.sh"
  cp -f "$REPO_ROOT/scripts/lib/inner-parallelism.sh" "$fixture/scripts/lib/inner-parallelism.sh"
  cp -f "$REPO_ROOT/scripts/lib/harness-reap.sh" "$fixture/scripts/lib/harness-reap.sh"
  cp -f "$REPO_ROOT/scripts/push-gate-lock-lib.sh" "$fixture/scripts/push-gate-lock-lib.sh"

  awk -v events_q="$events_q" '
    /^case "\$mode" in$/ {
      print "case \"$mode\" in"
      print "  serial-fail-fast)"
      print "    add_job \"serial-fails-first\" \"printf '\''%s\\n'\'' serial-fails-first >> " events_q "; exit 42\""
      print "    add_job \"serial-queued-after-failure\" \"printf '\''%s\\n'\'' serial-queued-after-failure >> " events_q "; exit 0\""
      print "    ;;"
      print "  parallel-fail-fast)"
      print "    add_job \"parallel-fails-first\" \"for i in {1..100}; do grep -qx parallel-already-running " events_q " && break; sleep 0.05; done; grep -qx parallel-already-running " events_q " || exit 99; printf '\''%s\\n'\'' parallel-fails-first >> " events_q "; exit 42\""
      print "    add_job \"parallel-already-running\" \"printf '\''%s\\n'\'' parallel-already-running >> " events_q "; for i in {1..100}; do grep -qx parallel-fails-first " events_q " && break; sleep 0.05; done; grep -qx parallel-fails-first " events_q " || exit 99; printf '\''%s\\n'\'' parallel-already-running-done >> " events_q "\""
      print "    add_job \"parallel-queued-after-failure\" \"printf '\''%s\\n'\'' parallel-queued-after-failure >> " events_q "; exit 0\""
      print "    ;;"
      print "  *)"
      print "    usage"
      print "    exit 1"
      print "    ;;"
      print "esac"
      skip=1
      next
    }
    skip && /^esac$/ {
      skip=0
      next
    }
    !skip { print }
  ' "$LOCAL_PARALLEL" >"$fixture/scripts/test-local-parallel"
  chmod +x "$fixture/scripts/test-local-parallel"
}

run_fixture() {
  local fixture="$1" mode="$2" jobs="$3" out="$4"
  mkdir -p "$fixture/logs-$mode"
  local runner=("$fixture/scripts/test-local-parallel")
  if [[ -n "${GC_TEST_LOCAL_PARALLEL_BASH:-}" ]]; then
    runner=("$GC_TEST_LOCAL_PARALLEL_BASH" "$fixture/scripts/test-local-parallel")
  fi

  set +e
  GC_TEST_NO_SLICE=1 \
    GC_PUSH_GATE_NO_CAP=1 \
    GC_TEST_NO_ORPHAN_SWEEP=1 \
    LOCAL_TEST_JOBS="$jobs" \
    LOCAL_TEST_LOG_DIR="$fixture/logs-$mode" \
    "${runner[@]}" "$mode" >"$out" 2>&1
  local status=$?
  set -e

  printf '%s' "$status"
}

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/gc-local-fail-fast.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

EVENTS="$WORK/events"
EVENTS_Q="$(printf '%q' "$EVENTS")"
FIXTURE="$WORK/fixture"
mkdir -p "$FIXTURE"
build_fixture_runner "$FIXTURE" "$EVENTS_Q"

SERIAL_OUT="$WORK/serial.out"
: >"$EVENTS"
serial_status="$(run_fixture "$FIXTURE" serial-fail-fast 1 "$SERIAL_OUT")"
serial_events="$(cat "$EVENTS")"
serial_output="$(cat "$SERIAL_OUT")"
assert_eq "serial.exit_status_preserves_failure" "$serial_status" "42"
assert_contains "serial.started_failing_job" "$serial_events" "serial-fails-first"
assert_not_contains "serial.did_not_start_queued_job_after_failure" "$serial_events" "serial-queued-after-failure"
assert_contains "serial.progress_is_top_level" "$serial_output" "test-local-progress "
assert_contains "serial.progress_reports_skipped_count" "$serial_output" "skipped=1"

PARALLEL_OUT="$WORK/parallel.out"
: >"$EVENTS"
parallel_status="$(run_fixture "$FIXTURE" parallel-fail-fast 2 "$PARALLEL_OUT")"
parallel_events="$(cat "$EVENTS")"
parallel_output="$(cat "$PARALLEL_OUT")"
assert_eq "parallel.exit_status_preserves_first_failure" "$parallel_status" "42"
assert_contains "parallel.started_failing_job" "$parallel_events" "parallel-fails-first"
assert_contains "parallel_preserves_already_running_job" "$parallel_events" "parallel-already-running"
assert_contains "parallel.drains_already_running_job" "$parallel_events" "parallel-already-running-done"
assert_not_contains "parallel.did_not_start_queued_job_after_failure" "$parallel_events" "parallel-queued-after-failure"
assert_contains "parallel.progress_is_top_level" "$parallel_output" "test-local-progress "
assert_contains "parallel.progress_reports_skipped_count" "$parallel_output" "skipped=1"

echo
echo "local-fail-fast tests: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
