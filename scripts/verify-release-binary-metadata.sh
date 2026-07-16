#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <gc-binary> <expected-commit> [expected-version]" >&2
}

if [[ $# -lt 2 || $# -gt 3 ]]; then
  usage
  exit 2
fi

binary=$1
expected_commit=$2
expected_version=${3:-}

if [[ ! -x "$binary" ]]; then
  echo "ERROR: release binary is not executable: $binary" >&2
  exit 1
fi
if [[ -z "$expected_commit" ]]; then
  echo "ERROR: expected commit must not be empty" >&2
  exit 1
fi

version_json=$("$binary" version --json --long)
actual_commit=$(jq -er \
  '.commit | if type == "string" and length > 0 then . else error("missing commit") end' \
  <<<"$version_json")
actual_version=$(jq -er \
  '.version | if type == "string" and length > 0 then . else error("missing version") end' \
  <<<"$version_json")

if [[ "$actual_commit" == *-dirty ]]; then
  echo "ERROR: release binary reports a dirty commit: $actual_commit" >&2
  exit 1
fi
if [[ "$actual_commit" != "$expected_commit" ]]; then
  echo "ERROR: release binary commit is $actual_commit, expected $expected_commit" >&2
  exit 1
fi
if [[ -n "$expected_version" && "$actual_version" != "$expected_version" ]]; then
  echo "ERROR: release binary version is $actual_version, expected $expected_version" >&2
  exit 1
fi

build_info=$(go version -m "$binary")
vcs_revision=$(awk '
  $1 == "build" && $2 ~ /^vcs\.revision=/ {
    sub(/^vcs\.revision=/, "", $2)
    print $2
    exit
  }
' <<<"$build_info")
vcs_modified=$(awk '
  $1 == "build" && $2 ~ /^vcs\.modified=/ {
    sub(/^vcs\.modified=/, "", $2)
    print $2
    exit
  }
' <<<"$build_info")
module_version=$(awk '$1 == "mod" { print $3; exit }' <<<"$build_info")

if [[ "$vcs_revision" != "$expected_commit" ]]; then
  echo "ERROR: embedded vcs.revision is ${vcs_revision:-missing}, expected $expected_commit" >&2
  exit 1
fi
if [[ "$vcs_modified" != "false" ]]; then
  echo "ERROR: embedded vcs.modified is ${vcs_modified:-missing}, expected false" >&2
  exit 1
fi
if [[ "$module_version" == *+dirty ]]; then
  echo "ERROR: embedded module version is dirty: $module_version" >&2
  exit 1
fi

echo "release binary metadata: OK (version=$actual_version commit=$actual_commit vcs.modified=false)"
