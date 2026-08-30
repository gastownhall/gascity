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
timeout_s="${BEADS_HOOK_TIMEOUT:-300}"
used_perl=0

set +e
if command -v timeout >/dev/null 2>&1; then
  timeout "$timeout_s" bd hooks run "$hook_name" "$@"
  status=$?
elif command -v gtimeout >/dev/null 2>&1; then
  gtimeout "$timeout_s" bd hooks run "$hook_name" "$@"
  status=$?
elif command -v perl >/dev/null 2>&1; then
  used_perl=1
  perl -e 'alarm shift; exec @ARGV' "$timeout_s" bd hooks run "$hook_name" "$@"
  status=$?
else
  echo >&2 "beads: hook '$hook_name' running without timeout; install coreutils or perl to enable BEADS_HOOK_TIMEOUT"
  bd hooks run "$hook_name" "$@"
  status=$?
fi
set -e

# A wedged bd must not wedge every commit in the repo. `timeout` reports 124;
# the perl fallback surfaces the same condition as SIGALRM (142).
if [ "$status" -eq 124 ] || { [ "$used_perl" -eq 1 ] && [ "$status" -eq 142 ]; }; then
  echo >&2 "beads: hook '$hook_name' timed out after ${timeout_s}s — continuing without beads"
  status=0
fi

# Exit 3 means this clone has no beads database — not a rejection.
if [ "$status" -eq 3 ]; then
  echo >&2 "beads: database not initialized — skipping hook '$hook_name'"
  status=0
fi

exit "$status"
