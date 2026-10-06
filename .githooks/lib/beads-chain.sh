#!/usr/bin/env sh
# Forward one git hook invocation to beads.
#
# Only one directory can own core.hooksPath. Beads' installer claims it for
# .beads/hooks, and those hooks exec `bd hooks run <hook>` without chaining
# onward — so while beads owned the path, every gate in .githooks (staged-Go
# formatting, lint-changed, the three codegen+stage steps, make vet, the
# push-time suite) was silently skipped, and spec-derived drift reached the
# mainline. .githooks is the single owner instead, and calls beads from here so
# both keep running.
#
# The exit-code carve-outs below intentionally mirror the beads integration
# block, so moving ownership changes nothing about how beads itself behaves.
#
# Usage: .githooks/lib/beads-chain.sh <hook-name> [hook-args...]
set -eu

hook_name="${1:?beads-chain: hook name required}"
shift

# Contributors without beads installed still get the repo's own gates.
command -v bd >/dev/null 2>&1 || exit 0

export BD_GIT_HOOK=1

# Mirrors the beads hook block as of beads 766e6c17f (#5522, 2026-08-10):
# validated timeout, GNU-only helper selection, `--` argv separation.
# Re-diff against cmd/bd/hooks.go generateHookSection when bumping bd.
#
# An unvalidated duration is the sharp edge: `timeout abc bd ...` exits 125,
# which this chain would propagate as a hook rejection, so one typo'd env var
# would block every commit in the clone. Fall back to the default instead.
timeout_s="${BEADS_HOOK_TIMEOUT-300}"
case "$timeout_s" in
  *[!0-9]*|'') timeout_invalid=1 ;;
  *[1-9]*)     timeout_invalid=0 ;;
  *)           timeout_invalid=1 ;;
esac
if [ "$timeout_invalid" -eq 1 ]; then
  echo >&2 "beads: invalid BEADS_HOOK_TIMEOUT; using 300 seconds"
  timeout_s=300
fi

# Windows timeout.exe shares the name with an incompatible command line
# (beads #5503), so accept only GNU coreutils implementations.
timeout_backend=none
timeout_cmd=
for candidate in timeout gtimeout; do
  if command -v "$candidate" >/dev/null 2>&1; then
    if candidate_version="$("$candidate" --version 2>/dev/null)"; then
      case "$candidate_version" in
        "timeout (GNU coreutils) "*) timeout_cmd=$candidate; break ;;
      esac
    fi
  fi
done

set +e
if [ -n "$timeout_cmd" ]; then
  timeout_backend=coreutils
  "$timeout_cmd" -- "$timeout_s" bd hooks run "$hook_name" ${1+"$@"}
  status=$?
elif command -v perl >/dev/null 2>&1; then
  timeout_backend=perl
  perl -e 'alarm shift; exec @ARGV' -- "$timeout_s" bd hooks run "$hook_name" ${1+"$@"}
  status=$?
else
  echo >&2 "beads: hook '$hook_name' running without timeout; install coreutils or perl to enable BEADS_HOOK_TIMEOUT"
  bd hooks run "$hook_name" ${1+"$@"}
  status=$?
fi
set -e

# A wedged bd must not wedge every commit in the repo. `timeout` reports 124;
# the perl fallback surfaces the same condition as SIGALRM (142). Only swallow
# the code the backend we actually used would report, so a bd that exits 124 or
# 142 on its own still counts as a rejection.
if { [ "$timeout_backend" = coreutils ] && [ "$status" -eq 124 ]; } || \
   { [ "$timeout_backend" = perl ] && [ "$status" -eq 142 ]; }; then
  echo >&2 "beads: hook '$hook_name' timed out after ${timeout_s}s — continuing without beads"
  status=0
fi

# Exit 3 means this clone has no beads database — not a rejection.
if [ "$status" -eq 3 ]; then
  echo >&2 "beads: database not initialized — skipping hook '$hook_name'"
  status=0
fi

exit "$status"
