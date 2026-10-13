#!/usr/bin/env bash
# check-generated-in-sync.sh - run a generator into a scratch tree and compare
# what it wrote with the committed copies. The body of the Bazel
# generated-artifact drift tests (//cmd/genspec:genspec_in_sync_test,
# //cmd/genschema:genschema_in_sync_test).
#
# Usage:
#   check-generated-in-sync.sh FIX_HINT PATCH_NAME [--source DIR ...] \
#       GENERATOR [ARG ...] -- REPO_PATH COMMITTED [REPO_PATH COMMITTED ...]
#
# GENERATOR runs with a scratch directory as its working directory, the way
# the repo's generators run from the repository root: they write their
# outputs at repository-relative paths, and they recognize the root by its
# go.mod, so the scratch root carries an empty go.mod marker. Each --source
# DIR (repository-relative, present in the runfiles) is copied into the
# scratch root for generators that read source files. GENERATOR and any ARG
# naming an existing file are made absolute first.
#
# Each REPO_PATH is a file the generator writes (repository-relative) and
# COMMITTED is the checked-in copy. On drift the script prints the
# regeneration diff, writes it as PATCH_NAME (a patch `git apply` takes from
# the repository root) under $TEST_UNDECLARED_OUTPUTS_DIR when Bazel provides
# one, and exits 1.
set -euo pipefail

usage() {
	echo "usage: $0 FIX_HINT PATCH_NAME [--source DIR ...] GENERATOR [ARG ...] -- REPO_PATH COMMITTED [...]" >&2
	exit 2
}

[ "$#" -ge 3 ] || usage
fix_hint="$1"
patch_name="$2"
shift 2

abs() {
	if [ -e "$1" ]; then
		printf '%s/%s' "$(cd "$(dirname "$1")" && pwd)" "$(basename "$1")"
	else
		printf '%s' "$1"
	fi
}

scratch="$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/generated-in-sync.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/root"
: >"$scratch/root/go.mod"

while [ "$#" -gt 0 ] && [ "$1" = "--source" ]; do
	[ "$#" -ge 2 ] && [ -d "$2" ] || {
		echo "check-generated-in-sync: --source ${2:-} is not a directory" >&2
		exit 2
	}
	mkdir -p "$scratch/root/$(dirname "$2")"
	cp -RL "$2" "$scratch/root/$2"
	shift 2
done

generator=()
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
	generator+=("$(abs "$1")")
	shift
done
[ "$#" -gt 0 ] || usage
shift
[ "${#generator[@]}" -gt 0 ] && [ "$#" -gt 0 ] && [ $(($# % 2)) -eq 0 ] || usage

if ! (cd "$scratch/root" && "${generator[@]}") >"$scratch/generator.log" 2>&1; then
	echo "check-generated-in-sync: generator failed: ${generator[*]}" >&2
	cat "$scratch/generator.log" >&2
	exit 1
fi

patch="$scratch/$patch_name"
: >"$patch"
stale=()
while [ "$#" -gt 0 ]; do
	repo_path="$1" committed="$2"
	shift 2
	generated="$scratch/root/$repo_path"
	if [ ! -f "$generated" ]; then
		echo "check-generated-in-sync: generator did not write $repo_path" >&2
		exit 1
	fi
	if [ ! -f "$committed" ]; then
		echo "check-generated-in-sync: committed $repo_path not found at $committed" >&2
		exit 1
	fi
	if ! cmp -s "$committed" "$generated"; then
		stale+=("$repo_path")
		{
			printf 'diff --git a/%s b/%s\n' "$repo_path" "$repo_path"
			diff -u --label "a/$repo_path" --label "b/$repo_path" "$committed" "$generated" || true
		} >>"$patch"
	fi
done

if [ "${#stale[@]}" -eq 0 ]; then
	echo "check-generated-in-sync: generated files are fresh."
	exit 0
fi

echo "check-generated-in-sync: STALE generated files:" >&2
printf '  %s\n' "${stale[@]}" >&2
echo "Fix: run '$fix_hint' and commit the result." >&2
if [ -n "${TEST_UNDECLARED_OUTPUTS_DIR:-}" ]; then
	cp -f "$patch" "$TEST_UNDECLARED_OUTPUTS_DIR/$patch_name"
	echo "Regeneration patch: test.outputs/$patch_name" >&2
fi
echo "--- regeneration diff ---" >&2
cat "$patch" >&2
exit 1
