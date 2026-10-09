#!/usr/bin/env bash
# fetch.sh [pin|pin-client]   fetch, verify, check out (outputs dir, sha)
# fetch.sh verify PINFILE     verify only: the pinned commit is in rbe-worker main's history
#
# The repository URL is fixed here; a pin file names a commit, never a
# repository. Anonymous HTTPS, refs/heads/main only: no token reaches git or
# the tree, and a commit reachable only from a fork is never fetched, so it
# fails below. Fails closed. Never prints the scaler's isolation-phase
# marker ("isolation: "), which NLPOOL_BREAKER_ISOLATION_ROLLBACK watches for
# (nor a commit subject, which could contain it).
#
# Hardened against the fetched repository itself: no system or global git
# config, no LFS smudge filter, no hook runs (core.hooksPath=/dev/null), and
# only https is an allowed transport (protocol.allow=never,
# protocol.https.allow=always) except for RBE_WORKER_TEST_ALLOW_FILE=1
# (scripts/rbe_worker_shim_test.go's local file:// fixture only; unset in
# every real invocation, so a real run can never reach a file:// URL this
# way).
#
# bash-3.2-safe: no mapfile, no timeout. gascity's script tests run this on
# macOS's /bin/bash (bash 3.2), per scripts/rbe_worker_shim_test.go.
set -euo pipefail

export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_LFS_SKIP_SMUDGE=1
for v in $(env | grep -oE '^GIT_[A-Z0-9_]+' || true); do
	case "$v" in
	GIT_CONFIG_NOSYSTEM | GIT_CONFIG_GLOBAL | GIT_LFS_SKIP_SMUDGE | GIT_TERMINAL_PROMPT) ;;
	*) unset "$v" ;;
	esac
done
git_protocol_allow=(-c protocol.allow=never -c protocol.https.allow=always)
[ "${RBE_WORKER_TEST_ALLOW_FILE:-}" != 1 ] || git_protocol_allow+=(-c protocol.file.allow=always)
git() { command git -c core.hooksPath=/dev/null "${git_protocol_allow[@]}" "$@"; }

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
url=https://github.com/gastownhall/rbe-worker.git

mode=fetch
pinfile=pin
case "${1:-}" in
"" | pin | pin-client) pinfile=${1:-pin} ;;
verify) mode=verify pinfile=${2:?usage: fetch.sh verify pin|pin-client} ;;
*) echo "usage: fetch.sh [pin|pin-client] | verify pin|pin-client" >&2; exit 2 ;;
esac
case "$pinfile" in
pin | pin-client) ;;
*) echo "::error title=rbe-worker::bad pin file '$pinfile'"; exit 2 ;;
esac

# Cutover switch (D8, S3-S5 only): source in-tree returns this checkout's own
# tools/rbe instead of fetching rbe-worker. Only the worker pin takes
# in-tree; pin-client (the client mechanism) always fetches/verifies.
if [ "$mode" = fetch ] && [ "${SOURCE:-pinned}" = in-tree ] && [ "$pinfile" = pin ]; then
	[ -x "$GITHUB_WORKSPACE/tools/rbe/blacksmith-worker.sh" ] ||
		{ echo "::error title=rbe-worker::source in-tree, but this checkout has no tools/rbe"; exit 1; }
	{ echo "dir=$GITHUB_WORKSPACE/tools/rbe"; echo "sha=in-tree"; } >>"$GITHUB_OUTPUT"
	echo "rbe-worker: in-tree tools/rbe"
	exit 0
fi
case "${SOURCE:-pinned}" in
pinned | in-tree) ;;
*) echo "::error title=rbe-worker::source must be pinned or in-tree"; exit 2 ;;
esac

sha=$(grep -E '^[0-9a-f]{40}$' "$here/$pinfile" || true)
if [ -z "$sha" ] || [ "$(printf '%s\n' "$sha" | wc -l | tr -d ' ')" != 1 ]; then
	echo "::error title=rbe-worker::$pinfile needs exactly one 40-hex line"
	exit 2
fi

dir=${RUNNER_TEMP:?}/rbe-worker-$pinfile
rm -rf "$dir"
git init -q "$dir"

attempt=1
until GIT_TERMINAL_PROMPT=0 git -C "$dir" -c credential.helper= -c http.lowSpeedLimit=1000 \
	-c http.lowSpeedTime=60 fetch -q --no-tags "$url" +refs/heads/main:refs/remotes/origin/main; do
	[ "$attempt" -lt 3 ] || { echo "::error title=rbe-worker::cannot fetch $url main"; exit 1; }
	sleep $((attempt * 10))
	attempt=$((attempt + 1))
done

# cat-file -t, not -e "$sha^{commit}": -e peels a tag to the commit it
# names and accepts it, so a pin could name a tag, tree or blob object
# instead of the commit it resolves to. -t is strict: $sha itself must be
# a commit.
if [ "$(git -C "$dir" cat-file -t "$sha" 2>/dev/null || true)" != commit ] ||
	! git -C "$dir" merge-base --is-ancestor "$sha" refs/remotes/origin/main; then
	echo "::error title=rbe-worker::$sha is not in gastownhall/rbe-worker main's history; refused"
	exit 1
fi

if [ "$mode" = verify ]; then
	echo "rbe-worker: $pinfile $sha is on main"
	exit 0
fi

# Refuse a symlink before it is ever checked out (a checkout follows it;
# ls-tree reports the tree's mode/type without doing so): worker must be a
# directory, and worker/blacksmith-worker.sh must be a regular executable
# (mode 100755), never a symlink (120000) to somewhere outside this tree.
worker_entry=$(git -C "$dir" ls-tree "$sha" -- worker | awk '{print $1, $2}')
if [ "$worker_entry" != "040000 tree" ]; then
	echo "::error title=rbe-worker::$sha's worker is '$worker_entry', not a directory (symlink?); refused"
	exit 1
fi
script_entry=$(git -C "$dir" ls-tree -r "$sha" -- worker/blacksmith-worker.sh | awk '{print $1, $2}')
if [ "$script_entry" != "100755 blob" ]; then
	echo "::error title=rbe-worker::$sha's worker/blacksmith-worker.sh is '$script_entry', not mode 100755 (symlink?); refused"
	exit 1
fi
# The two checks above only cover worker/ itself and blacksmith-worker.sh;
# the worker's own setup (sudo install) follows symlinks anywhere under
# worker/, so any entry there with mode 120000 is refused too, wherever it
# is (e.g. worker/rbe-action-launch -> /outside).
symlink=$(git -C "$dir" ls-tree -r "$sha" -- worker | awk '$1 == "120000" { print $4; exit }')
if [ -n "$symlink" ]; then
	echo "::error title=rbe-worker::$sha's $symlink is a symlink; refused"
	exit 1
fi

git -C "$dir" -c advice.detachedHead=false checkout -q --detach "$sha"
if [ "$(git -C "$dir" rev-parse HEAD)" != "$sha" ] || [ ! -x "$dir/worker/blacksmith-worker.sh" ]; then
	echo "::error title=rbe-worker::$sha did not check out to a runnable worker/"
	exit 1
fi
{ echo "dir=$dir/worker"; echo "sha=$sha"; } >>"$GITHUB_OUTPUT"
# Sha and date only: never the commit subject, which could contain the
# scaler's isolation-phase marker ("isolation: ").
echo "rbe-worker: $sha ($(git -C "$dir" log -1 --format='%cs' "$sha"))"
echo "rbe-worker \`$sha\` (gastownhall/rbe-worker main)" >>"$GITHUB_STEP_SUMMARY"
