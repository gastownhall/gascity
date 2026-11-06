#!/usr/bin/env bash
# fetch.sh [pin|pin-client]   fetch, verify, check out (outputs dir, sha)
# fetch.sh verify PINFILE     verify only: the pinned commit is in rbe-worker main's history
#
# Canary (pool workflows' rbe_worker_sha input, by hand only): with
# RBE_WORKER_CANARY_SHA set, mode pin checks out that commit instead of the
# pin's, and only if it is 40-hex, on rbe-worker main's first-parent line
# (a merged commit, not one inside a merged branch) and a descendant of the
# pin. The URL, the refs and every tree check are the same. Outputs canary
# (true or false) and pin (the pin file's commit). The workflow runs h5.sh
# pin..sha before any fetched code.
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

sha=$(grep -E '^[0-9a-f]{40}$' "$here/$pinfile" || true)
if [ -z "$sha" ] || [ "$(printf '%s\n' "$sha" | wc -l | tr -d ' ')" != 1 ]; then
	echo "::error title=rbe-worker::$pinfile needs exactly one 40-hex line"
	exit 2
fi
pin_sha=$sha
canary=${RBE_WORKER_CANARY_SHA:-}
if [ -n "$canary" ]; then
	if [ "$mode" != fetch ] || [ "$pinfile" != pin ]; then
		echo "::error title=rbe-worker::a canary commit is for the pool worker's fetch (pin) alone"
		exit 2
	fi
	if ! printf '%s' "$canary" | grep -qE '^[0-9a-f]{40}$' || [ "$(printf '%s' "$canary" | wc -l | tr -d ' ')" != 0 ]; then
		echo "::error title=rbe-worker::the canary commit must be one 40-hex sha"
		exit 2
	fi
	sha=$canary
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

# A canary: a merge on main's first-parent line, and the pin's descendant
# (never a rollback, never a commit only reachable through a merged branch).
# rev-list's output is captured whole, then searched: under pipefail, a pipe
# into grep -q would kill rev-list with SIGPIPE once grep stops at the
# canary, and refuse a valid canary.
if [ -n "$canary" ]; then
	first_parent=$(git -C "$dir" rev-list --first-parent refs/remotes/origin/main) ||
		{ echo "::error title=rbe-worker::cannot list rbe-worker main's first-parent line"; exit 1; }
	if ! grep -qx "$sha" <<<"$first_parent"; then
		echo "::error title=rbe-worker::canary $sha is not on rbe-worker main's first-parent line; refused"
		exit 1
	fi
	if ! git -C "$dir" merge-base --is-ancestor "$pin_sha" "$sha"; then
		echo "::error title=rbe-worker::canary $sha does not descend from the pin $pin_sha; refused"
		exit 1
	fi
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
{
	echo "dir=$dir/worker"
	echo "sha=$sha"
	echo "pin=$pin_sha"
	if [ -n "$canary" ]; then echo "canary=true"; else echo "canary=false"; fi
} >>"$GITHUB_OUTPUT"
# Sha and date only: never the commit subject, which could contain the
# scaler's isolation-phase marker ("isolation: ").
echo "rbe-worker: $sha ($(git -C "$dir" log -1 --format='%cs' "$sha"))"
if [ -n "$canary" ]; then
	echo "rbe-worker canary \`$sha\` (gastownhall/rbe-worker main; pin \`$pin_sha\`)" >>"$GITHUB_STEP_SUMMARY"
else
	echo "rbe-worker \`$sha\` (gastownhall/rbe-worker main)" >>"$GITHUB_STEP_SUMMARY"
fi
