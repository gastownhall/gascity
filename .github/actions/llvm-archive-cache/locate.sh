#!/usr/bin/env bash
# Locate the hermetic LLVM release archive in setup-bazel's repository cache.
#
# MODULE.bazel's llvm_dist pins the archive by sha256, and Bazel stores a
# download pinned by sha256 in the repository cache's content-addressable
# store at content_addressable/sha256/<sha256>/. That directory gets a runner
# cache entry of its own, keyed on the digest (see action.yml).
#
# Env: REPO_CACHE (setup-bazel's --repository_cache), MODULE_BAZEL (default
# MODULE.bazel), RUNNER_OS. Outputs (to $GITHUB_OUTPUT when set, else
# stdout): path=<archive directory>, key=<cache key>.
set -euo pipefail

repo_cache="${REPO_CACHE:?REPO_CACHE is required}"
runner_os="${RUNNER_OS:?RUNNER_OS is required}"
module="${MODULE_BAZEL:-MODULE.bazel}"

# A read loop rather than mapfile: macOS runners run this under bash 3.2.
shas=()
while IFS= read -r sha; do
	shas+=("$sha")
done < <(sed -n '/^llvm_dist(/,/^)/s/^ *sha256 = "\([0-9a-f]\{64\}\)",$/\1/p' "$module")
if [[ "${#shas[@]}" -ne 1 ]]; then
	echo "llvm-archive-cache: want one sha256-pinned llvm_dist(...) in $module, found ${#shas[@]}" >&2
	exit 1
fi

out="${GITHUB_OUTPUT:-/dev/stdout}"
{
	echo "path=$repo_cache/content_addressable/sha256/${shas[0]}"
	echo "key=bazel-llvm-v1-$runner_os-${shas[0]}"
} >>"$out"
