#!/usr/bin/env bash
# go_test_run.bzl's action: run a go_test binary once, its log and outputs
# recorded. Usage: go_test_run.sh <exe> <log> <timeout> <test flags...>, with
# OUTS the "VAR=path ..." outputs the run gets the absolute paths of.
#
# A failing run fails the action, so no failure is cached. What it prints:
# the failing tests' summary lines (--- FAIL, a simulator seed's violated:
# and replay: lines), which a long report would push out of any tail, then
# the Go test timeout's panic block (it names the running test) when the run
# timed out, or else the log's tail; the simulator cache's reconcile lines
# are left out of both.
set -u
exe=$1 log=$2 timeout=$3
shift 3
export TEST_SRCDIR="$PWD/$exe.runfiles" TEST_TMPDIR="$PWD/$log.tmp"
export HOME="$TEST_TMPDIR" TMPDIR="$TEST_TMPDIR"
mkdir -p "$TEST_TMPDIR"
outs=${OUTS:-}
unset OUTS
for out in $outs; do
  export "${out%%=*}=$PWD/${out#*=}"
done
rc=0
timeout -k 10 "$timeout" "$exe" "$@" >"$log" 2>&1 || rc=$?
rm -rf "$TEST_TMPDIR"
if [ "$rc" -ne 0 ]; then
  filtered() { grep -v 'beads cache: reconciled' "$log"; }
  summary=$(filtered | grep -E -- '--- FAIL|violated:|replay:')
  if [ -n "$summary" ]; then
    printf 'go_test_run: failing:\n%s\n\n' "$summary" >&2
  fi
  if grep -q '^panic: test timed out' "$log"; then
    sed -n '/^panic: test timed out/,$p' "$log" | head -n 120 >&2
    echo "go_test_run: $exe $* outlived its -test.timeout" >&2
  else
    filtered | tail -n 400 >&2
    case $rc in
    124 | 137) echo "go_test_run: $exe $* outlived its timeout (${timeout}s), killed" >&2 ;;
    *) echo "go_test_run: $exe $* failed (exit $rc)" >&2 ;;
    esac
  fi
  exit 1
fi
for out in $outs; do
  [ -e "${out#*=}" ] || : >"${out#*=}"
done
