#!/usr/bin/env bash
# check-merge-integrity.sh [--base REF] [--head REF] [--lane REF] [--merged REF]
#                          [--allow FILE] [--repo DIR] [--self-test]
#
# A merge that compiles is not a merge that kept its meaning.
#
# Three integration merges of origin/main into this fork lost meaning silently
# and shipped:
#
#   ga-f7v2ft.167 — the merge RESURRECTED controllerDemandRouteTarget, a
#     predicate origin/main deleted in #5250 and replaced with the serving-rule
#     aware demandServableForTemplates. Six lane call sites kept the corpse
#     alive, so the tree compiled, and every ready epic and hold-parked row
#     spawned a pool seat that read empty and drained. Nothing was red.
#
#   the delete-source class — a conflict resolution that drops a behaviour
#     drops its tests in the same hunk, so the suite that would have failed
#     was deleted by the same commit that broke the code.
#
#   ga-f7v2ft.184 — the MIRROR of the first: the merge restored
#     readyDemandSnapshotFingerprint, which the LANE had retired when the scan
#     was promoted to the sweep's routed-work view. Upstream never touched the
#     function, so check 1 had nothing to compare; incoming upstream TESTS
#     called it, the merge broke the compile, and the compile was fixed by
#     taking the symbol back. It shipped with zero production callers.
#
# None is visible to the compiler, to `go vet`, or to a green test run. All
# three are visible as a SET DIFFERENCE against the merge base, which is what
# this guard computes.
#
# CHECK 1 — deleted-symbol resurrection.
#   Top-level Go symbols present at the merge base and absent from the
#   upstream head are symbols upstream RETIRED. None of them may exist in the
#   merged tree unless the allowlist names it with a reason. Tree-wide sets,
#   not per-file: a symbol that merely moved between files was never deleted.
#   Non-test sources only — check 2 owns the test files.
#
# CHECK 2 — vanished-test census.
#   Every Test* function present at the merge base must exist in the merged
#   tree, be named in a commit message on base..merged, or be listed in the
#   allowlist. A retirement anybody wrote down is fine; a retirement nobody
#   wrote down is the delete-source bug.
#
# CHECK 3 — restored lane deletion, the mirror of check 1.
#   Top-level Go symbols present at the merge base and absent from the LANE
#   (the merge's first parent) are symbols THIS BRANCH retired. None of them
#   may exist in the merged tree unless the allowlist names it with a reason.
#   Check 1 cannot see this class: when upstream never touched the symbol it is
#   byte-identical between base and head, so it is not an upstream retirement
#   and nothing compares it to anything. Symbols BOTH sides retired are
#   reported once, by check 1, which carries the stronger claim.
#
#   Check 3 needs a lane ref and therefore runs only when there is one: it is
#   supplied by --lane, or defaulted to ^1 in the merge-commit mode below. In
#   the pre-merge mode the lane IS the merged tree, so the set is empty by
#   construction and the check reports NOT APPLICABLE rather than passing mute.
#
# REF RESOLUTION, in order:
#   1. explicit --base/--head/--merged win (--lane is optional and enables
#      check 3).
#   2. if --merged is a merge commit, base = merge-base(^1, ^2),
#      head = ^2 (the side being merged in) and lane = ^1 (the branch that was
#      checked out). This is the post-merge audit.
#   3. otherwise head = origin/main and base = merge-base(merged, head). This
#      is the PRE-merge audit and asks the same question one commit earlier:
#      does this lane still carry symbols upstream has retired?
#
# When the resolved base and head are the same commit there is no upstream
# divergence to audit and the guard reports NOTHING TO AUDIT and passes. That
# is not a silent pass: it is printed, and it is the expected state on a lane
# that has not diverged from its base.
#
# HOW THIS RUNS — read this before trusting a green tree.
#   Nothing runs this guard for you. It is NOT in `make check`, NOT in any CI
#   workflow, and NOT in any git hook: `make check-merge-integrity` is the only
#   trigger and a human has to type it. So a green `make check`, a green
#   pre-commit and a green CI run all say NOTHING about the three classes
#   below. Run it by hand on every integration merge and before every release
#   cut. Wiring it into an automated gate is ga-f7v2ft.189 (council S1), which
#   also carries the four open soundness minors on this script.
#
#   RUN IT ON A COMMIT, NOT ON YOUR EDITS. Every tree here is materialized with
#   `git archive REF` (extract_tree), so the guard sees COMMITTED state only —
#   an uncommitted rename or deletion in your working tree is invisible to it.
#   A green run before you commit therefore certifies the tree you are ABOUT to
#   change, which is worth nothing. Commit first, then run; if it goes red, fix
#   it in a follow-up commit. Observed for real at ga-f7v2ft.161: a pre-commit
#   run passed check 2, and the very next commit's test rename took it from 34
#   vanished tests to 35.
#
# FAILS CLOSED on an unresolvable ref, an unreadable allowlist, a malformed
# allowlist line, an allowlist entry with no reason, or a symbol extraction
# that finds nothing at all. A guard that passes when it cannot evaluate
# manufactures false confidence. The one deliberate exception is a missing
# origin/main in mode 3 — a fresh clone without the remote fetched is not a
# merge, so the guard reports it and passes rather than failing a tree that is
# not integrating anything. NOTE that a SHALLOW origin/main resolves but has no
# merge base; deepen it (`git fetch --unshallow origin`) or pass --base
# explicitly rather than reading the refusal as a clean tree.
#
# SELF-TEST: `--self-test` proves the guard's bite on real temp git repos —
# a resurrection fails, an allowlisted resurrection passes, a moved symbol is
# not a deletion, a vanished test fails, a commit-body retirement passes, an
# allowlist entry without a reason fails, a restored lane deletion fails while
# the same tree without a lane ref passes, and a bad ref fails.
#
# TEMP SPACE: scratch trees go under /var/tmp, never the size-capped tmpfs on
# /tmp — three extracted Go trees are ~200MB and the fleet shares /tmp.

set -uo pipefail # intentionally NOT -e: run both checks and aggregate.

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
extractor="${script_dir}/lib/go-top-level-symbols.awk"

repo=""
base_ref=""
head_ref=""
lane_ref=""
merged_ref="HEAD"
allow_file=""
self_test=0

usage() {
	sed -n '2,12p' "${BASH_SOURCE[0]}"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--base)
		base_ref="${2:-}"
		shift 2
		;;
	--head)
		head_ref="${2:-}"
		shift 2
		;;
	--lane)
		lane_ref="${2:-}"
		shift 2
		;;
	--merged)
		merged_ref="${2:-}"
		shift 2
		;;
	--allow)
		allow_file="${2:-}"
		shift 2
		;;
	--repo)
		repo="${2:-}"
		shift 2
		;;
	--self-test)
		self_test=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "check-merge-integrity: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

scratch=""
cleanup() {
	if [ -n "$scratch" ] && [ -d "$scratch" ]; then
		rm -rf "$scratch"
	fi
}
trap cleanup EXIT

die() {
	echo "check-merge-integrity: $*" >&2
	exit 2
}

# extract_tree REF DEST populates DEST with the ref's Go sources and prints
# nothing. Fails closed when the ref does not resolve or carries no Go.
extract_tree() {
	local ref="$1" dest="$2"
	mkdir -p "$dest" || return 1
	git archive "$ref" -- '*.go' 2>/dev/null | tar -x -C "$dest" 2>/dev/null
	if [ -z "$(find "$dest" -name '*.go' -print -quit)" ]; then
		return 1
	fi
	return 0
}

# symbols_of DIR MODE prints the sorted unique symbol set of a tree.
# MODE=symbols reads non-test sources; MODE=tests reads _test.go only.
symbols_of() {
	local dir="$1" mode="$2"
	local find_expr=(-name '*.go')
	if [ "$mode" = "tests" ]; then
		find_expr=(-name '*_test.go')
	else
		find_expr=(-name '*.go' ! -name '*_test.go')
	fi
	(
		cd "$dir" || exit 1
		find . "${find_expr[@]}" -type f \
			! -path './vendor/*' ! -path '*/testdata/*' -print0 |
			xargs -0 -r awk -v mode="$mode" -f "$extractor" |
			cut -f2 | sort -u
	)
}

# symbol_paths DIR prints "symbol<TAB>path" for reporting where a retired
# symbol lived at the base.
symbol_paths() {
	local dir="$1"
	(
		cd "$dir" || exit 1
		find . -name '*.go' ! -name '*_test.go' -type f \
			! -path './vendor/*' ! -path '*/testdata/*' -print0 |
			xargs -0 -r awk -v mode=symbols -f "$extractor" |
			awk -F'\t' '{ print $2 "\t" $1 }' | sort -u
	)
}

# read_allowlist FILE SECTION prints the allowed names of one section and
# fails closed on any malformed line. Format, one entry per line:
#
#   symbol   <TAB> NAME <TAB> reason text
#   test     <TAB> NAME <TAB> reason text
#   restored <TAB> NAME <TAB> reason text
#
# The kinds are deliberately separate. `symbol` says "upstream retired this and
# the lane keeps it on purpose" (check 1); `restored` says "the lane retired
# this and the merge brings it back on purpose" (check 3). One kind for both
# would let a waiver written for one class silence the other.
#
# Blank lines and # comments are ignored. A missing or empty reason is a
# violation: an allowlist without reasons is a mute button.
read_allowlist() {
	local file="$1" section="$2"
	[ -f "$file" ] || return 1
	awk -F'\t' -v want="$section" '
		/^[[:space:]]*$/ { next }
		/^[[:space:]]*#/ { next }
		{
			if (NF < 3) {
				printf("malformed allowlist line %d: expected <kind>\\t<name>\\t<reason>\\n", NR) > "/dev/stderr"
				bad = 1
				next
			}
			kind = $1; name = $2; reason = $3
			for (i = 4; i <= NF; i++) { reason = reason "\t" $i }
			gsub(/^[ \t]+|[ \t]+$/, "", kind)
			gsub(/^[ \t]+|[ \t]+$/, "", name)
			gsub(/^[ \t]+|[ \t]+$/, "", reason)
			if (kind != "symbol" && kind != "test" && kind != "restored") {
				printf("unknown allowlist kind %s on line %d\n", kind, NR) > "/dev/stderr"
				bad = 1
				next
			}
			if (name == "" || reason == "") {
				printf("allowlist line %d has no %s\n", NR, (name == "" ? "name" : "reason")) > "/dev/stderr"
				bad = 1
				next
			}
			if (kind == want) { print name }
		}
		END { if (bad) exit 1 }
	' "$file"
}

run_check() {
	local exit_code=0

	if [ -n "$repo" ]; then
		cd "$repo" || die "cannot enter --repo $repo"
	fi
	git rev-parse --is-inside-work-tree >/dev/null 2>&1 ||
		die "not inside a git work tree"

	[ -r "$extractor" ] || die "symbol extractor is missing: $extractor"

	if [ -z "$allow_file" ]; then
		allow_file="${script_dir}/merge-integrity-allow.txt"
	fi
	[ -r "$allow_file" ] || die "allowlist is unreadable: $allow_file"

	local merged
	merged=$(git rev-parse --verify "${merged_ref}^{commit}" 2>/dev/null) ||
		die "cannot resolve --merged ref: $merged_ref"

	local parents parent_count
	parents=$(git rev-list --parents -n 1 "$merged")
	parent_count=$(($(echo "$parents" | wc -w) - 1))

	local mode
	if [ -n "$base_ref" ] || [ -n "$head_ref" ]; then
		[ -n "$base_ref" ] || die "--head given without --base"
		[ -n "$head_ref" ] || die "--base given without --head"
		mode="explicit"
	elif [ "$parent_count" -ge 2 ]; then
		head_ref="${merged}^2"
		base_ref=$(git merge-base "${merged}^1" "${merged}^2" 2>/dev/null) ||
			die "cannot compute merge base of ${merged_ref}'s parents"
		[ -n "$lane_ref" ] || lane_ref="${merged}^1"
		mode="merge-commit"
	else
		if ! git rev-parse --verify "origin/main^{commit}" >/dev/null 2>&1; then
			echo "check-merge-integrity: origin/main is not present; nothing to audit"
			return 0
		fi
		head_ref="origin/main"
		base_ref=$(git merge-base "$merged" origin/main 2>/dev/null) ||
			die "cannot compute merge base against origin/main"
		mode="pre-merge"
	fi

	local base head lane=""
	base=$(git rev-parse --verify "${base_ref}^{commit}" 2>/dev/null) ||
		die "cannot resolve --base ref: $base_ref"
	head=$(git rev-parse --verify "${head_ref}^{commit}" 2>/dev/null) ||
		die "cannot resolve --head ref: $head_ref"
	if [ -n "$lane_ref" ]; then
		lane=$(git rev-parse --verify "${lane_ref}^{commit}" 2>/dev/null) ||
			die "cannot resolve --lane ref: $lane_ref"
	fi

	echo "check-merge-integrity: mode=$mode"
	echo "  base   $base"
	echo "  head   $head"
	[ -n "$lane" ] && echo "  lane   $lane"
	echo "  merged $merged"

	if [ "$base" = "$head" ]; then
		echo "check-merge-integrity: base == head; NOTHING TO AUDIT"
		return 0
	fi

	scratch=$(mktemp -d -p /var/tmp check-merge-integrity.XXXXXX) ||
		die "cannot create scratch directory under /var/tmp"

	extract_tree "$base" "$scratch/base" || die "cannot read Go sources at base $base"
	extract_tree "$head" "$scratch/head" || die "cannot read Go sources at head $head"
	extract_tree "$merged" "$scratch/merged" || die "cannot read Go sources at merged $merged"

	symbols_of "$scratch/base" symbols >"$scratch/base.symbols"
	symbols_of "$scratch/head" symbols >"$scratch/head.symbols"
	symbols_of "$scratch/merged" symbols >"$scratch/merged.symbols"
	symbols_of "$scratch/base" tests >"$scratch/base.tests"
	symbols_of "$scratch/merged" tests >"$scratch/merged.tests"
	symbol_paths "$scratch/base" >"$scratch/base.paths"

	local censuses="base.symbols head.symbols merged.symbols base.tests merged.tests"
	if [ -n "$lane" ]; then
		extract_tree "$lane" "$scratch/lane" || die "cannot read Go sources at lane $lane"
		symbols_of "$scratch/lane" symbols >"$scratch/lane.symbols"
		censuses="$censuses lane.symbols"
	fi

	for census in $censuses; do
		if [ ! -s "$scratch/$census" ]; then
			die "symbol census $census is empty; refusing to pass on an unevaluated tree"
		fi
	done

	local allowed_symbols allowed_tests allowed_restored
	allowed_symbols=$(read_allowlist "$allow_file" symbol) ||
		die "allowlist is malformed: $allow_file"
	allowed_tests=$(read_allowlist "$allow_file" test) ||
		die "allowlist is malformed: $allow_file"
	allowed_restored=$(read_allowlist "$allow_file" restored) ||
		die "allowlist is malformed: $allow_file"
	printf '%s\n' "$allowed_symbols" | sed '/^$/d' | sort -u >"$scratch/allow.symbols"
	printf '%s\n' "$allowed_tests" | sed '/^$/d' | sort -u >"$scratch/allow.tests"
	printf '%s\n' "$allowed_restored" | sed '/^$/d' | sort -u >"$scratch/allow.restored"

	# CHECK 1: upstream-retired symbols that survived into the merged tree.
	comm -23 "$scratch/base.symbols" "$scratch/head.symbols" >"$scratch/retired.symbols"
	comm -12 "$scratch/retired.symbols" "$scratch/merged.symbols" >"$scratch/resurrected.all"
	comm -23 "$scratch/resurrected.all" "$scratch/allow.symbols" >"$scratch/resurrected"

	local retired_count resurrected_count
	retired_count=$(wc -l <"$scratch/retired.symbols" | tr -d ' ')
	resurrected_count=$(wc -l <"$scratch/resurrected" | tr -d ' ')
	echo "check-merge-integrity: check 1 — ${retired_count} symbol(s) retired upstream since the base"
	if [ "$resurrected_count" -gt 0 ]; then
		echo
		echo "DELETED-SYMBOL RESURRECTION (${resurrected_count}): upstream retired these and the merged tree still declares them." >&2
		while IFS= read -r sym; do
			local where
			where=$(awk -F'\t' -v s="$sym" '$1 == s { print $2 }' "$scratch/base.paths" | paste -sd, -)
			echo "  ${sym}  (at base: ${where:-unknown})" >&2
		done <"$scratch/resurrected"
		echo >&2
		echo "Port the upstream replacement into the lane call sites and delete the symbol," >&2
		echo "or add it to ${allow_file} with the reason the lane deliberately keeps it." >&2
		exit_code=1
	fi

	# CHECK 2: tests that existed at the base and exist nowhere now.
	comm -23 "$scratch/base.tests" "$scratch/merged.tests" >"$scratch/vanished.all"
	comm -23 "$scratch/vanished.all" "$scratch/allow.tests" >"$scratch/vanished.unallowed"

	: >"$scratch/vanished"
	if [ -s "$scratch/vanished.unallowed" ]; then
		git log --format=%B "${base}..${merged}" >"$scratch/bodies" 2>/dev/null || : >"$scratch/bodies"
		while IFS= read -r test_name; do
			if ! grep -qF -- "$test_name" "$scratch/bodies"; then
				echo "$test_name" >>"$scratch/vanished"
			fi
		done <"$scratch/vanished.unallowed"
	fi

	local vanished_count
	vanished_count=$(wc -l <"$scratch/vanished" | tr -d ' ')
	echo "check-merge-integrity: check 2 — $(wc -l <"$scratch/vanished.all" | tr -d ' ') test(s) present at base and absent now"
	if [ "$vanished_count" -gt 0 ]; then
		echo
		echo "VANISHED TESTS (${vanished_count}): present at the merge base, gone from the merged tree, retired nowhere." >&2
		while IFS= read -r test_name; do
			echo "  ${test_name}" >&2
		done <"$scratch/vanished"
		echo >&2
		echo "Restore the test, name it in a commit message on ${base_ref}..${merged_ref} with the reason," >&2
		echo "or add it to ${allow_file}." >&2
		exit_code=1
	fi

	# CHECK 3: lane-retired symbols the merge put back.
	if [ -z "$lane" ]; then
		echo "check-merge-integrity: check 3 — no lane ref; NOT APPLICABLE"
	else
		comm -23 "$scratch/base.symbols" "$scratch/lane.symbols" >"$scratch/retired.lane"
		comm -12 "$scratch/retired.lane" "$scratch/merged.symbols" >"$scratch/restored.all"
		# A symbol both sides retired is check 1's finding, not two findings.
		comm -23 "$scratch/restored.all" "$scratch/resurrected.all" >"$scratch/restored.mine"
		comm -23 "$scratch/restored.mine" "$scratch/allow.restored" >"$scratch/restored"

		local lane_retired_count restored_count
		lane_retired_count=$(wc -l <"$scratch/retired.lane" | tr -d ' ')
		restored_count=$(wc -l <"$scratch/restored" | tr -d ' ')
		echo "check-merge-integrity: check 3 — ${lane_retired_count} symbol(s) retired by the lane since the base"
		if [ "$restored_count" -gt 0 ]; then
			echo
			echo "RESTORED LANE DELETION (${restored_count}): the lane retired these and the merged tree declares them again." >&2
			while IFS= read -r sym; do
				local where
				where=$(awk -F'\t' -v s="$sym" '$1 == s { print $2 }' "$scratch/base.paths" | paste -sd, -)
				echo "  ${sym}  (at base: ${where:-unknown})" >&2
			done <"$scratch/restored"
			echo >&2
			echo "A merge that has to take a symbol back to compile is usually satisfying an" >&2
			echo "incoming TEST, not restoring behaviour the lane still ships: check for callers" >&2
			echo "before keeping it. Finish the lane's deletion and re-point or retire the tests," >&2
			echo "or add it to ${allow_file} with the reason the merge deliberately restores it." >&2
			exit_code=1
		fi
	fi

	if [ "$exit_code" -eq 0 ]; then
		echo "check-merge-integrity: OK"
	fi
	return "$exit_code"
}

# ---------------------------------------------------------------------------
# Self-test
# ---------------------------------------------------------------------------

st_repo=""
st_failures=0

st_git() {
	git -C "$st_repo" "$@" >/dev/null 2>&1
}

st_commit() {
	st_git add -A
	git -C "$st_repo" -c user.email=guard@test -c user.name=guard \
		commit -q -m "$1" >/dev/null 2>&1
}

st_expect() {
	local want="$1" label="$2"
	shift 2
	local out status
	out=$("$@" 2>&1)
	status=$?
	if [ "$want" = "pass" ] && [ "$status" -ne 0 ]; then
		echo "SELF-TEST FAIL: ${label}: expected pass, got exit ${status}" >&2
		echo "$out" | sed 's/^/    /' >&2
		st_failures=$((st_failures + 1))
	elif [ "$want" = "fail" ] && [ "$status" -eq 0 ]; then
		echo "SELF-TEST FAIL: ${label}: expected failure, got exit 0" >&2
		echo "$out" | sed 's/^/    /' >&2
		st_failures=$((st_failures + 1))
	else
		echo "  ok  ${label}"
	fi
}

st_write() {
	local path="$1"
	shift
	mkdir -p "$(dirname "${st_repo}/${path}")"
	printf '%s\n' "$@" >"${st_repo}/${path}"
}

self_test_main() {
	local root
	root=$(mktemp -d -p /var/tmp check-merge-integrity-selftest.XXXXXX) ||
		die "cannot create self-test root under /var/tmp"
	# shellcheck disable=SC2064 # expand root now, it is local to this function
	trap "rm -rf '$root'" RETURN

	st_repo="${root}/repo"
	mkdir -p "$st_repo"
	st_git init -q -b main
	st_git config user.email guard@test
	st_git config user.name guard

	# Base: two symbols and two tests.
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}' '' 'func Retired() {}'
	st_write pkg/a_test.go 'package pkg' '' 'import "testing"' '' \
		'func TestKept(t *testing.T) {}' '' 'func TestDoomed(t *testing.T) {}'
	st_commit base
	local base
	base=$(git -C "$st_repo" rev-parse HEAD)

	# Upstream head: Retired is deleted, TestDoomed still exists.
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}'
	st_commit upstream-deletes-retired
	local head
	head=$(git -C "$st_repo" rev-parse HEAD)

	local allow="${root}/allow.txt"
	printf '# self-test allowlist\n' >"$allow"

	local guard=("${BASH_SOURCE[0]}" --repo "$st_repo" --allow "$allow")

	# 1. Resurrection: the merged tree brings Retired back.
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}' '' 'func Retired() {}'
	st_commit merged-resurrects-retired
	local resurrect
	resurrect=$(git -C "$st_repo" rev-parse HEAD)
	st_expect fail "resurrected symbol fails" \
		"${guard[@]}" --base "$base" --head "$head" --merged "$resurrect"

	# 2. The same tree passes once the allowlist names it with a reason.
	printf 'symbol\tRetired\tlane keeps it deliberately for the self-test\n' >"$allow"
	st_expect pass "allowlisted resurrection passes" \
		"${guard[@]}" --base "$base" --head "$head" --merged "$resurrect"

	# 3. An allowlist entry with no reason fails closed.
	printf 'symbol\tRetired\n' >"$allow"
	st_expect fail "allowlist entry without a reason fails" \
		"${guard[@]}" --base "$base" --head "$head" --merged "$resurrect"
	printf '# self-test allowlist\n' >"$allow"

	# 4. A symbol that MOVED files was never deleted: no finding.
	st_git checkout -q "$base"
	st_git checkout -q -b moved
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}'
	st_write pkg/b.go 'package pkg' '' 'func Retired() {}'
	st_commit upstream-moves-retired
	local moved_head
	moved_head=$(git -C "$st_repo" rev-parse HEAD)
	st_expect pass "moved symbol is not a deletion" \
		"${guard[@]}" --base "$base" --head "$moved_head" --merged "$moved_head"

	# 5. Vanished test with no retirement note fails. The tree also adopts
	#    upstream's deletion so check 1 is silent and check 2 is what bites.
	st_git checkout -q "$base"
	st_git checkout -q -b vanished
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}'
	st_write pkg/a_test.go 'package pkg' '' 'import "testing"' '' \
		'func TestKept(t *testing.T) {}'
	st_commit "drop a test without saying why"
	local vanished
	vanished=$(git -C "$st_repo" rev-parse HEAD)
	st_expect fail "vanished test fails" \
		"${guard[@]}" --base "$base" --head "$head" --merged "$vanished"

	# 6. The same deletion passes when the commit body names the test.
	st_git checkout -q "$base"
	st_git checkout -q -b retired-in-body
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}'
	st_write pkg/a_test.go 'package pkg' '' 'import "testing"' '' \
		'func TestKept(t *testing.T) {}'
	st_commit "retire TestDoomed: its subject moved upstream"
	local retired_body
	retired_body=$(git -C "$st_repo" rev-parse HEAD)
	st_expect pass "commit-body retirement passes" \
		"${guard[@]}" --base "$base" --head "$head" --merged "$retired_body"

	# 7. Go source inside a raw string declares nothing. The merged tree
	#    embeds a fixture that spells `func Retired()` at column 0; a scanner
	#    that cannot see raw strings would call that a live resurrection.
	st_git checkout -q "$base"
	st_git checkout -q -b rawstring
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}' '' \
		'const fixture = `' 'func Retired() {}' '`'
	st_commit merged-embeds-fixture
	local rawstring
	rawstring=$(git -C "$st_repo" rev-parse HEAD)
	st_expect pass "raw-string Go is not a declaration" \
		"${guard[@]}" --base "$base" --head "$head" --merged "$rawstring"

	# 7b. A `//` inside a raw string must not end the line for the scanner.
	#     Getting this wrong desynchronizes the rest of the file: every
	#     declaration below the literal disappears from ALL THREE censuses, so
	#     the resurrection below goes unreported and the guard passes while
	#     lying. That is the exact defect the real tree exposed in
	#     internal/config/workquery.go — 22 declarations silently absent, then
	#     reported as upstream deletions. Every symbol here sits BELOW a
	#     jq-shaped literal, so only a scanner that tracks raw strings sees it.
	local jq='const filter = `[.[] | select((.assignee // "") == $id)] | .[:1]`'
	st_git checkout -q "$base"
	st_git checkout -q -b rawcomment
	st_write pkg/a.go 'package pkg' '' "$jq" '' 'func Kept() {}' '' 'func Retired() {}'
	st_commit base-with-jq-literal
	local jq_base
	jq_base=$(git -C "$st_repo" rev-parse HEAD)
	st_write pkg/a.go 'package pkg' '' "$jq" '' 'func Kept() {}'
	st_commit upstream-deletes-below-a-jq-literal
	local jq_head
	jq_head=$(git -C "$st_repo" rev-parse HEAD)
	st_write pkg/a.go 'package pkg' '' "$jq" '' 'func Kept() {}' '' 'func Retired() {}'
	st_commit merged-resurrects-below-a-jq-literal
	st_expect fail "a // inside a raw string does not hide the declarations below it" \
		"${guard[@]}" --base "$jq_base" --head "$jq_head" --merged "$(git -C "$st_repo" rev-parse HEAD)"

	# 8. CHECK 3: the lane retired a symbol and the merge brought it back.
	#    Upstream never touched it — head still declares it, and adds one of its
	#    own — so check 1 has nothing to compare and only the lane comparison
	#    can see the restoration. This is ga-f7v2ft.184 in miniature.
	st_git checkout -q "$base"
	st_git checkout -q -b lane-retires
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}'
	st_commit lane-retires-retired
	local lane_head
	lane_head=$(git -C "$st_repo" rev-parse HEAD)

	st_git checkout -q "$base"
	st_git checkout -q -b upstream-keeps
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}' '' 'func Retired() {}' '' 'func Added() {}'
	st_commit upstream-adds-a-symbol-and-keeps-retired
	local lane_upstream
	lane_upstream=$(git -C "$st_repo" rev-parse HEAD)

	st_git checkout -q "$lane_head"
	st_git checkout -q -b lane-merged
	st_write pkg/a.go 'package pkg' '' 'func Kept() {}' '' 'func Retired() {}' '' 'func Added() {}'
	st_commit merge-restores-the-lane-deletion
	local restored_merge
	restored_merge=$(git -C "$st_repo" rev-parse HEAD)
	st_expect fail "restored lane deletion fails" \
		"${guard[@]}" --base "$base" --head "$lane_upstream" --lane "$lane_head" \
		--merged "$restored_merge"

	# 8b. The same tree passes once the allowlist names it under the RESTORED
	#     kind — and the check-1 kind must not be the one that silences it.
	printf 'symbol\tRetired\ta check-1 waiver must not mute check 3\n' >"$allow"
	st_expect fail "a check-1 waiver does not silence check 3" \
		"${guard[@]}" --base "$base" --head "$lane_upstream" --lane "$lane_head" \
		--merged "$restored_merge"
	printf 'restored\tRetired\tthe merge deliberately restores it for the self-test\n' >"$allow"
	st_expect pass "allowlisted restoration passes" \
		"${guard[@]}" --base "$base" --head "$lane_upstream" --lane "$lane_head" \
		--merged "$restored_merge"
	printf '# self-test allowlist\n' >"$allow"

	# 8c. Controls. The SAME merged tree passes when the lane never retired the
	#     symbol (--lane base), and when no lane ref is given at all. Without
	#     these, case 8's failure could be any of the three checks firing.
	st_expect pass "a symbol the lane kept is not a restoration" \
		"${guard[@]}" --base "$base" --head "$lane_upstream" --lane "$base" \
		--merged "$restored_merge"
	st_expect pass "no lane ref makes check 3 not applicable" \
		"${guard[@]}" --base "$base" --head "$lane_upstream" --merged "$restored_merge"

	# 9. An unresolvable ref fails closed rather than passing vacuously.
	st_expect fail "unresolvable ref fails closed" \
		"${guard[@]}" --base "$base" --head "$head" --merged deadbeefdeadbeef
	st_expect fail "unresolvable lane ref fails closed" \
		"${guard[@]}" --base "$base" --head "$lane_upstream" --lane deadbeefdeadbeef \
		--merged "$restored_merge"

	# 10. An unreadable allowlist fails closed.
	st_expect fail "missing allowlist fails closed" \
		"${BASH_SOURCE[0]}" --repo "$st_repo" --allow "${root}/nope.txt" \
		--base "$base" --head "$head" --merged "$resurrect"

	if [ "$st_failures" -gt 0 ]; then
		echo "check-merge-integrity --self-test: ${st_failures} failure(s)" >&2
		return 1
	fi
	echo "check-merge-integrity --self-test: OK"
	return 0
}

if [ "$self_test" -eq 1 ]; then
	self_test_main
	exit $?
fi

run_check
exit $?
