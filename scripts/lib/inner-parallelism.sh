#!/usr/bin/env bash
# inner-parallelism.sh — computes GOFLAGS=-p=<n> for the go-test binaries
# launched by each outer test-local-parallel job (ga-04m84s).
#
# The outer job count (test-local-job-count) sizes concurrent shard
# processes; each shard's `go test` binary defaults its internal -p to
# GOMAXPROCS, so when multiple shards run concurrently they each
# independently try to claim the whole machine, oversubscribing it.
# gc_inner_parallelism divides the outer budget across however many
# shards are actually running concurrently so each one's -p is capped to
# its fair share instead.
#
# Scope: -p only bounds cross-package build/test-binary concurrency, not
# within-package t.Parallel() fan-out (that's the separate -parallel flag,
# also defaulting to GOMAXPROCS, which this fix does not set). Shards that
# invoke go test against a single package -- most of cmd/gc's job list --
# get -p bounded only for their dependency-build phase, not their
# t.Parallel() run phase; the multi-package jobs get the full benefit.
#
# Source this file in other scripts:
#   source "$repo_root/scripts/lib/inner-parallelism.sh"

# gc_inner_parallelism LOCAL_JOBS JOB_COUNT prints the -p value each
# concurrent job should pass to `go test`. GC_TEST_INNER_P overrides the
# computation outright (must be a positive integer) for deterministic tests.
gc_inner_parallelism() {
  local local_jobs="$1" job_count="$2"

  if [[ -n "${GC_TEST_INNER_P:-}" ]]; then
    [[ "$GC_TEST_INNER_P" =~ ^[0-9]+$ && "$GC_TEST_INNER_P" -gt 0 ]] ||
      { echo "GC_TEST_INNER_P must be a positive integer" >&2; return 1; }
    printf '%s\n' "$GC_TEST_INNER_P"
    return
  fi

  local effective_outer="$job_count"
  if (( local_jobs < effective_outer )); then
    effective_outer="$local_jobs"
  fi
  local inner_p=$(( local_jobs / effective_outer ))
  if (( inner_p < 1 )); then
    inner_p=1
  fi
  printf '%s\n' "$inner_p"
}
