#!/usr/bin/env bash
# The push-time test suite, called by .githooks/pre-push when Go sources change.
#
# `bazel test //...` hashes every action exactly like CI does (the committed
# .bazelrc pins every key-affecting flag; scripts/bazel_key_parity_test.go),
# so a push reuses what pre-push, PR and main runs already computed. Three
# tiers (TESTING.md "Bazel cache tiers"):
#
#   contributor  --config=fork-cache: rbe-west's anonymous read-only cache;
#                misses run on this machine and nothing is uploaded.
#   maintainer   --config=remote-exec: .bazelrc.local carries a remote
#                executor and the maintainer's mTLS client certificate.
#   CI           the trusted writer (bazel-test.yml); never this script.
#
# GC_PREPUSH_SUITE picks the mode (default auto):
#   auto   remote-exec when bazel is installed and .bazelrc.local sets
#          `build:remote-exec --remote_executor=...`; fork-cache when bazel is
#          installed without one and .bazelrc's pinned test PATH has `go`;
#          make test-fast-parallel otherwise.
#   rbe    --config=remote-exec.
#   cache  --config=fork-cache.
#   go     make test-fast-parallel (plain go test, the pre-Bazel suite).
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

mode="${GC_PREPUSH_SUITE:-auto}"

# A maintainer's credential: a non-comment build:remote-exec line naming a
# non-empty --remote_executor in the gitignored .bazelrc.local.
remote_executor_configured() {
  [ -f .bazelrc.local ] &&
    grep -Eq '^[[:space:]]*build:remote-exec[[:space:]](.*[[:space:]])?--remote_executor(=|[[:space:]]+)[^[:space:]]' .bazelrc.local
}

have_bazel() {
  command -v bazel >/dev/null 2>&1
}

# .bazelrc's pinned test PATH (the last unconditional one, as Bazel applies it).
pinned_test_path() {
  [ -f .bazelrc ] && sed -n 's/^test[[:space:]]\{1,\}--test_env=PATH=\([^[:space:]]\{1,\}\).*/\1/p' .bazelrc | tail -n 1
}

# Locally executed tests (fork-cache misses) exec `go` from the pinned test
# PATH; without it there, they fail rather than skip.
pinned_path_has_go() {
  local pinned dir
  pinned="$(pinned_test_path)" || return 0
  [ -n "$pinned" ] || return 0
  local IFS=:
  for dir in $pinned; do
    [ -x "$dir/go" ] && return 0
  done
  return 1
}

case "$mode" in
auto)
  if ! have_bazel; then
    echo "pre-push: bazel is not installed; running make test-fast-parallel." >&2
    echo "pre-push: install bazelisk to reuse CI's cached test results (TESTING.md \"Bazel cache tiers\")." >&2
    mode=go
  elif remote_executor_configured; then
    mode=rbe
  elif ! pinned_path_has_go; then
    echo "pre-push: no go on .bazelrc's pinned test PATH ($(pinned_test_path)); running make test-fast-parallel." >&2
    echo "pre-push: link your GOROOT to /usr/local/go to run the bazel suite (TESTING.md \"Bazel cache tiers\")." >&2
    mode=go
  else
    mode=cache
  fi
  ;;
go | rbe | cache) ;;
*)
  echo "pre-push: GC_PREPUSH_SUITE=$mode is not one of auto, rbe, cache, go" >&2
  exit 2
  ;;
esac

case "$mode" in
go)
  exec make test-fast-parallel
  ;;
rbe) config=remote-exec ;;
cache) config=fork-cache ;;
esac

if ! have_bazel; then
  echo "pre-push: GC_PREPUSH_SUITE=$mode needs bazel on PATH; install bazelisk or use GC_PREPUSH_SUITE=go" >&2
  exit 2
fi

# .bazelrc roots every test's tmpdir at /tmp/bt and nothing else creates it.
mkdir -p /tmp/bt
echo "pre-push: bazel test //... --config=$config (GC_PREPUSH_SUITE=go runs plain go test instead)" >&2
exec bazel test //... "--config=$config" --keep_going
