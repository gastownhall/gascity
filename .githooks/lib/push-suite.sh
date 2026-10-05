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
#
# Remote execution also needs a current worker-env pin (worker_env_refusal
# below): rbe-west's workers serve only main's pin. On a stale pin, or an open
# drift issue for main's pin, auto picks the non-remote mode and rbe fails.
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

# run_bounded SECONDS CMD...: CMD, killed after SECONDS where timeout(1)
# exists (stock macOS has none; there CMD runs unbounded).
run_bounded() {
  local seconds=$1
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$seconds" "$@"
  else
    "$@"
  fi
}

# github_repo REMOTE: REMOTE's owner/name when its configured URL is on
# github.com; fails otherwise.
github_repo() {
  local url
  url="$(git config --get "remote.$1.url")" || return 1
  url="${url%.git}"
  case "$url" in
  https://github.com/*/* | git@github.com:*/* | ssh://git@github.com/*/*)
    printf '%s\n' "${url#*github.com[:/]}"
    ;;
  *) return 1 ;;
  esac
}

# Why remote execution must not run, on stdout; nothing when the worker-env
# pin is current. Every remote action sends //platforms:rbe_worker's
# worker-env pin, and rbe-west's oss schedulers match it exactly against what
# live workers advertise: main's pin, nothing else (TESTING.md "Bazel cache
# tiers"). Any other pin, from a branch that predates a re-pin or moves the
# pin, queues every action forever while the pool scaler starts VMs that
# cannot take them; so does main's own pin while it has an open drift issue.
# Main is a fresh fetch of $GC_PREPUSH_MAIN_REMOTE (default origin), or its
# last fetched ref when offline; the drift issue lookup (gh, through
# tools/rbe/worker-env-drift preflight) is best effort and time-bounded.
worker_env_refusal() {
  local main_remote="${GC_PREPUSH_MAIN_REMOTE:-origin}" drift=tools/rbe/worker-env-drift
  local ref pin base repo out issue status=0
  ref="refs/remotes/$main_remote/main"
  if ! run_bounded 30 env GIT_TERMINAL_PROMPT=0 git fetch --no-tags --quiet "$main_remote" "+refs/heads/main:$ref" >/dev/null 2>&1; then
    echo "pre-push: could not fetch $main_remote's main; comparing against the last fetched $main_remote/main" >&2
  fi
  if ! git rev-parse --quiet --verify "$ref^{commit}" >/dev/null; then
    echo "there is no $main_remote/main to read rbe-west's current worker-env pin from (GC_PREPUSH_MAIN_REMOTE names the remote carrying gascity's main)"
    return 0
  fi
  if ! pin="$("$drift" pin 2>&1)"; then
    echo "this checkout's worker-env pin is unreadable: $pin"
    return 0
  fi
  if ! base="$(git show "$ref:platforms/BUILD.bazel" 2>/dev/null | "$drift" pin /dev/stdin 2>&1)"; then
    echo "$main_remote/main's worker-env pin is unreadable: $base"
    return 0
  fi
  if [ "$pin" != "$base" ]; then
    echo "this checkout pins worker-env=$pin, but rbe-west's workers serve only $main_remote/main's $base. Rebase onto main to execute remotely; a change that moves the pin runs its remote suite after merge"
    return 0
  fi
  if ! command -v gh >/dev/null 2>&1 || ! repo="$(github_repo "$main_remote")"; then
    return 0
  fi
  out="$(GITHUB_REPOSITORY="$repo" WORKER_ENV_REMOTE=true GITHUB_OUTPUT='' GITHUB_STEP_SUMMARY='' \
    run_bounded 20 "$drift" preflight platforms/BUILD.bazel /dev/null 2>&1)" || status=$?
  issue="$(printf '%s\n' "$out" | sed -n 's/^::error title=rbe worker-env drift::\(https:[^ :]*\):.*/\1/p')"
  if [ "$status" -eq 1 ] && [ -n "$issue" ]; then
    echo "main's worker-env pin $pin has an open drift issue, $issue: no live worker serves it until the re-pin lands"
  elif [ "$status" -ne 0 ] || printf '%s\n' "$out" | grep -q '^::warning title=rbe worker-env preflight::'; then
    echo "pre-push: could not check $repo's rbe worker-env drift issues (gh); executing remotely anyway" >&2
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
      refusal="$(worker_env_refusal)"
      if [ -n "$refusal" ]; then
        echo "pre-push: not executing remotely on $executor: $refusal." >&2
        executor=""
      fi
    fi
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
    refusal="$(worker_env_refusal)"
    if [ -n "$refusal" ]; then
      echo "pre-push: GC_PREPUSH_SUITE=rbe refused: $refusal. Its actions would queue on $executor with no worker to run them; use GC_PREPUSH_SUITE=cache|go meanwhile." >&2
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
