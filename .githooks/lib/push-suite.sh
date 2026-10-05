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
#   maintainer   --config=remote-exec: some rc file names a remote executor
#                and the maintainer's mTLS client certificate, either
#                .bazelrc.local's build:remote-exec lines or a machine rc
#                (agent hosts set `build --remote_executor=...` in ~/.bazelrc).
#   CI           the trusted writer (bazel-test.yml); never this script.
#
# GC_PREPUSH_SUITE picks the mode (default auto):
#   auto   remote-exec when bazel is installed and its effective options name
#          a remote executor; fork-cache when bazel is installed without one
#          and .bazelrc's pinned test PATH has `go`; make test-fast-parallel
#          otherwise.
#   rbe    --config=remote-exec; fails when no rc names an executor (the suite
#          would otherwise compile and run on this machine at --jobs=64).
#   cache  --config=fork-cache (it resets any rc's executor).
#   go     make test-fast-parallel (plain go test, the pre-Bazel suite).
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

mode="${GC_PREPUSH_SUITE:-auto}"
executor=""

# The remote executor some rc file names for this workspace, empty for none:
# the last non-empty --remote_executor among the rc options Bazel reads
# (system, workspace with .bazelrc.local, home) and the remote-exec config's
# definitions, from `bazel info --announce_rc --config=remote-exec`. Bazel's
# own reading of every rc, not a grep of one file, so an agent host's
# ~/.bazelrc executor counts. Other configs' definitions are skipped:
# fork-cache's --remote_executor= reset never applies to the remote-exec run.
# (Bazel lists config expansions after all rc sections although it applies
# them in place, so the listing gives no reliable "last value wins".) `info`
# inherits build options, contacts no remote and starts (or reuses) the
# server the suite then runs in. Fails, printing Bazel's error, when Bazel
# cannot read its options.
effective_remote_executor() {
  local announced
  if ! announced="$(bazel info --announce_rc --config=remote-exec release 2>&1 >/dev/null)"; then
    printf '%s\n' "$announced" >&2
    return 1
  fi
  printf '%s\n' "$announced" | awk '
    /^INFO: (Reading rc options|Options provided by the client)/ { section = 1; next }
    /^[^[:space:]]/ { section = 0 }
    !section && !/^INFO: Found applicable config definition [^ ]*:remote-exec / { next }
    {
      for (i = 1; i <= NF; i++) {
        value = ""
        if ($i ~ /^--remote_executor=/) {
          value = substr($i, length("--remote_executor=") + 1)
        } else if ($i == "--remote_executor" && i < NF) {
          value = $(i + 1)
        }
        if (value != "") {
          executor = value
        }
      }
    }
    END { print executor }'
}

# Sets $executor, or fails the push: an unreadable option set is no evidence
# for either mode.
probe_executor() {
  if ! executor="$(effective_remote_executor)"; then
    echo "pre-push: bazel info could not read this workspace's options (above); fix the rc, or set GC_PREPUSH_SUITE=go|rbe|cache" >&2
    exit 2
  fi
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
  else
    probe_executor
    if [ -n "$executor" ]; then
      mode=rbe
    elif ! pinned_path_has_go; then
      echo "pre-push: no go on .bazelrc's pinned test PATH ($(pinned_test_path)); running make test-fast-parallel." >&2
      echo "pre-push: link your GOROOT to /usr/local/go to run the bazel suite (TESTING.md \"Bazel cache tiers\")." >&2
      mode=go
    else
      mode=cache
    fi
  fi
  ;;
go | cache) ;;
rbe)
  if have_bazel; then
    probe_executor
    if [ -z "$executor" ]; then
      echo "pre-push: GC_PREPUSH_SUITE=rbe but no rc file names a --remote_executor; configure one (TESTING.md \"Bazel cache tiers\") or use GC_PREPUSH_SUITE=cache|go" >&2
      exit 2
    fi
  fi
  ;;
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
echo "pre-push: bazel test //... --config=$config${executor:+ on $executor} (GC_PREPUSH_SUITE=go runs plain go test instead)" >&2
exec bazel test //... "--config=$config" --keep_going
