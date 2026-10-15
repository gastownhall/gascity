#!/usr/bin/env bash
# Whether this host can run what a local `bazel test` builds, shared by
# .githooks/pre-commit (docs) and .githooks/lib/push-suite.sh (pre-push).
#
# Bazel-built test binaries load the ICU runtime that MODULE.bazel's pinned
# sysroot ships (the libicu package named in cc_sysroot_noble_amd64). A host
# whose dynamic loader cannot resolve it fails every locally executed test
# at startup, so the hooks pick the go twin instead of launching Bazel.
#
# Sourced, never executed: defining functions is all it does (no set
# changes, no commands run). bash 3.2 compatible; safe under `set -euo
# pipefail`. scripts/bazel_local_runtime_pin_test.go keeps the ICU major
# below, MODULE.bazel's pin and engdocs/bazel-quickstart.md in step.

local_runtime_icu_major=74

# The ICU shared objects the pinned sysroot retains, space separated.
local_runtime_sonames() {
  local base out=""
  for base in i18n uc data; do
    out="$out${out:+ }libicu$base.so.$local_runtime_icu_major"
  done
  printf '%s\n' "$out"
}

# run_bounded SECONDS CMD...: CMD in its own process group, which is killed
# after SECONDS (exit 124). timeout(1), else Homebrew's gtimeout, else perl
# (stock macOS has neither timeout); unbounded only without all three.
run_bounded() {
  local seconds=$1
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$seconds" "$@"
  elif command -v gtimeout >/dev/null 2>&1; then
    gtimeout "$seconds" "$@"
  elif command -v perl >/dev/null 2>&1; then
    perl -e '
      my $seconds = shift;
      my $pid = fork;
      defined $pid or die "fork: $!\n";
      if (!$pid) { setpgrp(0, 0); exec { $ARGV[0] } @ARGV; exit 127 }
      $SIG{ALRM} = sub { kill "TERM", -$pid; kill "CONT", -$pid; exit 124 };
      alarm $seconds;
      waitpid $pid, 0;
      exit($? & 127 ? 128 + ($? & 127) : $? >> 8);
    ' "$seconds" "$@"
  else
    "$@"
  fi
}

# local_runtime_probe_run CARRIER SONAMES CANARY SECONDS: preloads SONAMES and
# a CANARY that cannot exist into CARRIER and reads the dynamic loader's own
# "cannot be preloaded" complaints from stderr. The exit status says nothing
# (the loader ignores a failed preload), so it is never the verdict; the
# canary's complaint proves the loader reports preload failures at all. Prints
# one line and returns 0: `ok`, `missing <sonames>` (input order, only the
# missing ones) or `unknown <reason>`.
local_runtime_probe_run() {
  local carrier=$1 sonames=$2 canary=$3 seconds=$4
  local out status=0 name missing=""
  if ! command -v timeout >/dev/null 2>&1 && ! command -v gtimeout >/dev/null 2>&1 && ! command -v perl >/dev/null 2>&1; then
    echo "unknown no timeout, gtimeout or perl to bound the loader probe"
    return 0
  fi
  if [ ! -x "$carrier" ]; then
    echo "unknown no probe carrier at $carrier"
    return 0
  fi
  out="$(run_bounded "$seconds" env -i LC_ALL=C LD_PRELOAD="$sonames $canary" "$carrier" 2>&1 >/dev/null)" || status=$?
  if [ "$status" -eq 124 ]; then
    echo "unknown loader probe timed out after ${seconds}s"
    return 0
  elif [ "$status" -ne 0 ]; then
    echo "unknown probe carrier exited $status"
    return 0
  fi
  case "$out" in
  *"object '$canary' from LD_PRELOAD cannot be preloaded"*) ;;
  *)
    echo "unknown loader did not report preload failures"
    return 0
    ;;
  esac
  for name in $sonames; do
    case "$out" in
    *"object '$name' from LD_PRELOAD cannot be preloaded"*) missing="$missing${missing:+ }$name" ;;
    esac
  done
  if [ -n "$missing" ]; then
    echo "missing $missing"
  else
    echo ok
  fi
}

# local_runtime_probe: this host's verdict on the pinned ICU runtime, one line,
# always returning 0: `ok`, `missing <sonames>`, `unknown <reason>` or
# `n/a <os>/<arch>` (the pinned sysroot only serves Linux x86_64; other hosts
# build against their own libraries).
local_runtime_probe() {
  local os arch carrier=""
  if ! os="$(uname -s 2>/dev/null)" || ! arch="$(uname -m 2>/dev/null)" || [ -z "$os" ] || [ -z "$arch" ]; then
    echo "unknown could not read this host's platform from uname"
    return 0
  fi
  if [ "$os" != Linux ] || [ "$arch" != x86_64 ]; then
    echo "n/a $os/$arch"
    return 0
  fi
  for carrier in /usr/bin/true /bin/true; do
    [ -x "$carrier" ] && break
  done
  local_runtime_probe_run "$carrier" "$(local_runtime_sonames)" libgascity_loader_canary.so.0 10
}

# local_runtime_assess: runs local_runtime_probe once and sets
# $local_runtime_state to ok (usable, or not applicable here), missing or
# unknown, and $local_runtime_detail to the missing sonames or the reason.
# Empty, malformed or multi-line probe output is unknown.
local_runtime_assess() {
  local line
  local_runtime_state=unknown
  local_runtime_detail="probe produced no usable result"
  line="$(local_runtime_probe 2>/dev/null)" || return 0
  case "$line" in
  *$'\n'*) ;;
  ok | "n/a "?*)
    local_runtime_state=ok
    local_runtime_detail=""
    ;;
  "missing "?*)
    local_runtime_state=missing
    local_runtime_detail="${line#missing }"
    ;;
  "unknown "?*) local_runtime_detail="${line#unknown }" ;;
  esac
}

# The reason a hook runs the go twin, from the last local_runtime_assess.
local_runtime_why() {
  if [ "$local_runtime_state" = missing ]; then
    printf 'this host cannot load %s, the ICU runtime that MODULE.bazel'"'"'s sysroot pins (libicu%s) and every Bazel-built test binary needs, so a local bazel test fails at startup' \
      "$local_runtime_detail" "$local_runtime_icu_major"
  else
    printf 'could not verify that this host can load the libicu%s runtime that MODULE.bazel pins for Bazel-built test binaries: %s' \
      "$local_runtime_icu_major" "$local_runtime_detail"
  fi
}

# How to get a Bazel run back, common to both hooks; each hook adds its own
# remote-executor and override spellings.
local_runtime_resolve_hint() {
  printf 'make libicu%s resolvable to the loader (a side-by-side libicu%s install, or a private runtime with bazel --batch)' \
    "$local_runtime_icu_major" "$local_runtime_icu_major"
}
