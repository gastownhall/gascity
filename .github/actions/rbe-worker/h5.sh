#!/usr/bin/env bash
# h5.sh WORKER_REPO_DIR OLD NEW   (H5, D12)
#
# Every commit in OLD..NEW (exclusive..inclusive, git rev-list order) must
# be: GitHub-verified and signed; committed by web-flow, GitHub's
# merge-button identity (a direct push, however it is signed, is never
# web-flow); and have an associated pull request merged into
# gastownhall/rbe-worker's main (a signed web-flow commit with no merged PR
# is still refused). OLD empty or the all-zero placeholder: no range yet,
# the first pin. NEW not a descendant of OLD: a warning (a rollback), not a
# failure, so reverting a bad rbe-worker commit isn't blocked on rewriting
# rbe-worker's own history.
#
# Needs $GH_TOKEN (gh) and WORKER_REPO_DIR's git history (the caller's
# fetch of refs/heads/main). bash-3.2-safe, same as fetch.sh: no mapfile.
set -euo pipefail

# Not ${2:?...}: old is legitimately empty (the first pin, below), and
# ${var:?msg} treats empty the same as unset.
[ $# -eq 3 ] || { echo "usage: h5.sh WORKER_REPO_DIR OLD NEW" >&2; exit 2; }
worker_repo=$1
old=$2
new=$3
repo=gastownhall/rbe-worker

is_sha() { printf '%s' "$1" | grep -qE '^[0-9a-f]{40}$'; }

is_sha "$new" || { echo "::error title=rbe-worker bump::NEW '$new' is not a 40-hex sha"; exit 2; }
case "$old" in
"" | 0000000000000000000000000000000000000000)
	echo "rbe-worker: first pin ($new), no range to check (S3a)"
	exit 0
	;;
*)
	is_sha "$old" || { echo "::error title=rbe-worker bump::OLD '$old' is not a 40-hex sha"; exit 2; }
	;;
esac

# A missing base commit means rev-list below would fail or, worse, run
# against the wrong range: check it explicitly, before the loop, so a
# broken base pin fails closed instead of silently checking zero commits.
git -C "$worker_repo" cat-file -e "$old^{commit}" ||
	{ echo "::error title=rbe-worker bump::base pin $old is not in rbe-worker main's fetched history"; exit 1; }

git -C "$worker_repo" merge-base --is-ancestor "$old" "$new" ||
	echo "::warning title=rbe-worker bump::$new does not descend from $old (a rollback?): say why in the PR"

range=$(git -C "$worker_repo" rev-list "$old..$new")
for c in $range; do
	commit_json=$(gh api "repos/$repo/commits/$c")
	verified=$(printf '%s' "$commit_json" | jq -r '.commit.verification.verified')
	[ "$verified" = true ] ||
		{ echo "::error title=rbe-worker bump::$c is not a verified, signed commit"; exit 1; }
	committer=$(printf '%s' "$commit_json" | jq -r '.committer.login // ""')
	[ "$committer" = web-flow ] ||
		{ echo "::error title=rbe-worker bump::$c's committer is '$committer', not web-flow (a direct push bypasses review)"; exit 1; }
	merged=$(gh api "repos/$repo/commits/$c/pulls" --jq '[.[] | select(.merged_at != null and .base.ref == "main")] | length')
	[ "${merged:-0}" -gt 0 ] ||
		{ echo "::error title=rbe-worker bump::$c has no pull request merged into main"; exit 1; }
done
echo "rbe-worker: $old..$new ($(printf '%s\n' "$range" | grep -c .) commit(s)) all signed, verified, web-flow, merged"
