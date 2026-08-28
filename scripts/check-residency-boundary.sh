#!/usr/bin/env bash
# check-residency-boundary.sh [repo-root] [--emit-baseline] [--self-test]
#
# The eleventh blind reader must be unwritable.
#
# The typed-store split shipped a PLACEMENT contract (one authoritative accessor
# per class for "which store OWNS creates of class C"). It never shipped a
# LOOKUP contract, and "which stores answer this question, in what order, with
# what failure semantics" was hand-rolled at ~90 call sites built from two
# binding-blind base enumerators. internal/storeref's residency resolver is now
# that contract; this guard is what stops a ninety-first site from being
# written.
#
# WHAT IT SCANS: non-test .go under cmd/gc, internal/api, internal/sling and
# internal/dispatch, for the store-enumeration vocabulary declared in
# scripts/residency-boundary-patterns.txt — four families:
#
#   (a) base-enumerator consumption — BeadStores(), rigBeadStores(),
#       coordClassStoreCandidates(, workAssignmentStores(,
#       openAllConvoyStoresAt(, openConvoyStores(, openSourceWorkflowStores(,
#       convoyStoreCandidates(. None of these ever contains a class binding, so
#       every list built from one is binding-blind by construction.
#   (b) direct binding-gate re-derivation — graphClassBinding(,
#       routes.storeFor(, storeref.ClassCandidates(. Asking "is this class
#       relocated" a second time is how the split-store bug class reproduces
#       (#5125, #5127).
#   (c) hand-rolled probe lists and plan disassembly — a []beads.Store{ literal,
#       and reaching a PlanLeg's .Leg.Store directly. The executor
#       (ResolveOwner/Union) is the only place leg order and per-leg error policy
#       are applied; a literal list skips both, and so does a consumer that runs
#       a plan's legs itself. Zero hits today, so the first one is a violation.
#       WorkLegsForID is the same idea one level up: a plane that implements
#       storeref.WorkAxisRouter tells the resolver to use the plane's leg order
#       instead of its own, so a second implementation is a second answer to the
#       same question. The rule governing that — one axis per plane, and a
#       cross-plane agreement pin the day a second appears — is stated in
#       scripts/residency-boundary-patterns.txt beside the row.
#   (d) bespoke residence probes — IDInNamespace(, bdIDIsClassReserved(,
#       ReservedClassPrefix(. The namespace gate is the resolver's, and a site
#       that re-derives it is one `gc storage migrate` away from ga-axin6.
#
# THE RATCHET: scripts/residency-boundary-baseline.txt pins today's census as
# `path <TAB> enclosing function <TAB> pattern <TAB> count`. The check fails on any hit ABOVE the pinned
# count (a new blind reader, unwritable from day one) and on any pinned count
# the tree no longer reaches (the baseline must SHRINK in the same commit that
# retires a site). Shrink-only, like the resource-census ratchet.
#
# WHY COUNTS-PER-FUNCTION AND NOT LINE NUMBERS: a file:line baseline fails on
# every edit ABOVE a pinned line, which turns the guard into a false-positive
# generator for unrelated work and trains reviewers to rubber-stamp the marker.
# Counts are immune to line drift and still forbid growth. Keyed by file ALONE
# they were also maskable — a new site paired with a removal in the same file
# kept the count level, and since family (a) is consumption-shaped the
# signature-level AST half could not see it either. The enclosing function is
# therefore part of the key.
#
# THE HONEST RESIDUAL: a swap WITHIN one function of one pattern — deleting one
# `BeadStores()` call and adding another in the same function — still keeps the
# count level. That is a much smaller hole than the file-level one, it is
# visible in the diff of a single function, and the AST half additionally
# catches any new store-list SIGNATURE, which is the shape a genuinely new
# resolver-bypassing helper takes.
#
# SELF-TEST: `--self-test` proves the guard's own bite on real temp trees (new
# site fails, marker suppresses, stale baseline fails, fail-closed cases fail).
# `make check-residency-boundary` runs it before the real check.
#
# ESCAPE HATCH: a trailing `// residency:allow <reason>` on the line, same
# discipline as `// boundary:allow`. Regenerating the baseline to absorb a new
# hit defeats the ratchet: the diff is the review.
#
# FAILS CLOSED: a missing scan directory, an unreadable baseline, or a census
# that finds nothing at all is a violation, not a pass. A guard that silently
# passes when it cannot evaluate manufactures false confidence.

set -uo pipefail # intentionally NOT -e: run every check and aggregate.

emit_baseline=0
self_test=0
repo_root=""
for arg in "$@"; do
	case "$arg" in
	--emit-baseline) emit_baseline=1 ;;
	--self-test) self_test=1 ;;
	*) repo_root="$arg" ;;
	esac
done
if [[ -z "$repo_root" ]]; then
	repo_root=$(cd "$(dirname "$0")/.." && pwd)
fi

baseline_file="$repo_root/scripts/residency-boundary-baseline.txt"

# Directories the lookup contract governs.
scan_dirs=(cmd/gc internal/api internal/sling internal/dispatch)

# Genuine exceptions, each with a reason:
#   residency_topology.go  — the topology constructors themselves (§1.2).
#   infra_class_migrate.go
#   cmd_storage.go         — migration and status must enumerate stores BY
#                            DEFINITION; they are the tools that create topology.
#   cmd_bd_topology.go     — fork-only work-axis workspace routing, orthogonal
#                            to class residency.
allowlist=(
	cmd/gc/residency_topology.go
	internal/api/residency_topology.go
	cmd/gc/infra_class_migrate.go
	cmd/gc/cmd_storage.go
	cmd/gc/cmd_bd_topology.go
)

# The forbidden vocabulary is DATA, read from residency-boundary-patterns.txt.
# The Go half of the guard reads the same file, so the two cannot drift about
# what is forbidden — which would be this guard's own bug class, one level up.
patterns_file="$repo_root/scripts/residency-boundary-patterns.txt"
pattern_names=()
pattern_regexes=()
load_patterns() {
	local line name regex
	if [[ ! -r "$patterns_file" ]]; then
		note "pattern file $patterns_file is missing or unreadable"
		return 1
	fi
	while IFS=$'\t' read -r name regex; do
		[[ -z "${name:-}" || "${name:0:1}" == "#" ]] && continue
		[[ -z "${regex:-}" ]] && continue
		pattern_names+=("$name")
		pattern_regexes+=("$regex")
	done <"$patterns_file"
	((${#pattern_names[@]})) || { note "pattern file declares no pattern"; return 1; }
	return 0
}

failed=0
note() { echo "check-residency-boundary: $*" >&2; }

# --self-test proves this script's own BITE on real on-disk trees before it is
# trusted to police the repository. A guard nothing asserts against can be
# defanged silently by an edit to one regex, and the four cases below are the
# four ways this one is meant to fail. It runs no Go and spawns only copies of
# itself, so it costs the test-resource census nothing.
run_self_test() {
	local tmp status out rc=0
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' RETURN

	fixture() { # fixture <dir> <baseline-body>
		mkdir -p "$1"/{cmd/gc,internal/api,internal/sling,internal/dispatch,scripts}
		cp "$patterns_file" "$1/scripts/residency-boundary-patterns.txt"
		printf 'package main\n\nfunc a() { _ = BeadStores() }\n' >"$1/cmd/gc/existing.go"
		printf '%s' "$2" >"$1/scripts/residency-boundary-baseline.txt"
	}
	expect() { # expect <want-rc> <label> <root>
		out=$("$0" "$3" 2>&1)
		status=$?
		if [[ "$status" -ne "$1" ]]; then
			echo "SELF-TEST: $2: exit $status, want $1" >&2
			printf '%s\n' "$out" >&2
			rc=1
		fi
	}

	fixture "$tmp/clean" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\n'
	expect 0 "a tree matching its baseline must pass (the ratchet is not fail-always)" "$tmp/clean"

	fixture "$tmp/new" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\n'
	printf 'package main\n\nfunc b() { _ = rigBeadStores() }\n' >"$tmp/new/cmd/gc/eleventh.go"
	expect 1 "a NEW enumeration site must fail" "$tmp/new"

	# The mask a file-keyed baseline let through: delete one call and add a
	# different one in ANOTHER function of the SAME file. Per-file counts stay
	# level, and family (a) is consumption-shaped so no signature changes —
	# nothing but the enclosing-function key can see this.
	fixture "$tmp/swap" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\ncmd/gc/swap.go\tkeeper\ta:BeadStores\t1\n'
	printf 'package main\n\nfunc keeper() { _ = BeadStores() }\n\nfunc other() {}\n' >"$tmp/swap/cmd/gc/swap.go"
	expect 0 "the pre-swap tree must be clean" "$tmp/swap"
	printf 'package main\n\nfunc keeper() {}\n\nfunc other() { _ = BeadStores() }\n' >"$tmp/swap/cmd/gc/swap.go"
	expect 1 "a new consumption site paired with a removal in the SAME file must fail" "$tmp/swap"

	fixture "$tmp/marked" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\n'
	printf 'package main\n\nfunc b() { _ = rigBeadStores() } // residency:allow tested escape hatch\n' >"$tmp/marked/cmd/gc/marked.go"
	expect 0 "the residency:allow marker must suppress a hit" "$tmp/marked"

	fixture "$tmp/stale" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\ncmd/gc/gone.go\tgone\ta:BeadStores\t1\n'
	expect 1 "a baseline entry the tree no longer reaches must force a shrink" "$tmp/stale"

	fixture "$tmp/nobase" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\n'
	rm -f "$tmp/nobase/scripts/residency-boundary-baseline.txt"
	expect 1 "a missing baseline must fail closed" "$tmp/nobase"

	fixture "$tmp/nodir" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\n'
	rm -rf "$tmp/nodir/internal/api"
	expect 1 "a missing scan directory must fail closed" "$tmp/nodir"

	fixture "$tmp/comment" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\n'
	printf 'package main\n\n// b would call rigBeadStores() but only in prose.\nfunc b() {}\n' >"$tmp/comment/cmd/gc/prose.go"
	expect 0 "a comment naming the vocabulary is not a site" "$tmp/comment"

	# THE ROGUE CONSUMER. storeref.EachLeg is the sanctioned enumeration seam, so
	# a consumer that takes the legs and then reads them in its OWN order with
	# its OWN error handling names no forbidden symbol: no []beads.Store literal,
	# no .Leg.Store, no base enumerator. It passed both halves of this guard
	# before c:storeref.EachLeg was ratcheted. A third consumer must be a
	# reviewed baseline change, not a silent one.
	fixture "$tmp/rogue" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\n'
	printf 'package main\n\nfunc rogue(p storeref.ResolvedPlan) {\n\tstoreref.EachLeg(p, func(leg storeref.Leg, _ storeref.Role, _ storeref.ErrPolicy) {\n\t\t_, _ = leg.Store.Get("ga-1")\n\t})\n}\n' >"$tmp/rogue/cmd/gc/rogue.go"
	expect 1 "a NEW storeref.EachLeg consumer must fail" "$tmp/rogue"

	# And its control: the same consumer, baselined, passes — so the row above
	# ratchets growth rather than banning the seam the two real consumers need.
	fixture "$tmp/rogue-baselined" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\ncmd/gc/rogue.go\trogue\tc:storeref.EachLeg\t1\n'
	printf 'package main\n\nfunc rogue(p storeref.ResolvedPlan) {\n\tstoreref.EachLeg(p, func(leg storeref.Leg, _ storeref.Role, _ storeref.ErrPolicy) {\n\t\t_, _ = leg.Store.Get("ga-1")\n\t})\n}\n' >"$tmp/rogue-baselined/cmd/gc/rogue.go"
	expect 0 "a BASELINED storeref.EachLeg consumer must pass" "$tmp/rogue-baselined"

	# THE SECOND WORK AXIS. A plane that implements storeref.WorkAxisRouter
	# replaces the resolver's own by-id work rule with its own leg order. One
	# such plane exists; a second written silently is two answers to one
	# question, which is this guard's whole subject wearing the resolver's API.
	fixture "$tmp/axis" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\n'
	printf 'package main\n\ntype rogueAxis struct{}\n\nfunc (rogueAxis) WorkLegsForID(id string) []storeref.Leg { return nil }\n' >"$tmp/axis/internal/api/axis.go"
	expect 1 "a SECOND WorkAxisRouter implementation must fail" "$tmp/axis"

	fixture "$tmp/axis-baselined" $'cmd/gc/existing.go\ta\ta:BeadStores\t1\ninternal/api/axis.go\tWorkLegsForID\tc:WorkAxisRouter\t1\n'
	printf 'package main\n\ntype rogueAxis struct{}\n\nfunc (rogueAxis) WorkLegsForID(id string) []storeref.Leg { return nil }\n' >"$tmp/axis-baselined/internal/api/axis.go"
	expect 0 "a BASELINED WorkAxisRouter must pass" "$tmp/axis-baselined"

	return $rc
}

is_allowlisted() {
	local rel="$1"
	local entry
	for entry in "${allowlist[@]}"; do
		[[ "$rel" == "$entry" ]] && return 0
	done
	return 1
}

# census prints `path <TAB> function <TAB> pattern <TAB> count`, sorted.
#
# WHY THE FUNCTION IS PART OF THE KEY: a count keyed by file alone masks a
# consumption site that is PAIRED with a removal — delete one rigBeadStores()
# call in city_runtime.go, add a different one in another function of the same
# file, and the count is level. Family (a) is the bulk of this baseline and is
# consumption-shaped, so the signature-level AST half cannot see that swap
# either. Keying by enclosing function shrinks the hole to a swap WITHIN one
# function, which is the honest residual and is small enough that the diff shows
# it.
#
# ENCLOSING FUNCTION is read the way gofmt guarantees it can be: a top-level
# declaration starts at column 0, and only a top-level body closes with a `}` at
# column 0, so a state machine over those two lines attributes every line to its
# top-level function (a closure's hits belong to the function containing it) or
# to (file-scope). `make fmt-check` is what keeps that guarantee true. The Go
# half runs the identical rule, so the two halves key the same way by
# construction.
#
# A comment line, and a line bearing the `// residency:allow` marker, is not a
# site. One awk pass over every scanned file rather than one grep per (file,
# pattern): the guard runs on every commit, and a quadratic scan is a guard
# people disable.
census() {
	local dir
	local -a present=()
	for dir in "${scan_dirs[@]}"; do
		[[ -d "$repo_root/$dir" ]] && present+=("$repo_root/$dir")
	done
	((${#present[@]})) || return 0
	find "${present[@]}" -type f -name '*.go' ! -name '*_test.go' -print0 |
		sort -z |
		xargs -0 --no-run-if-empty awk -v patfile="$patterns_file" '
			BEGIN {
				npat = 0
				while ((getline line < patfile) > 0) {
					if (line ~ /^#/ || line ~ /^[ \t]*$/) continue
					tab = index(line, "\t")
					if (tab == 0) continue
					npat++
					pname[npat] = substr(line, 1, tab - 1)
					pregex[npat] = substr(line, tab + 1)
				}
				close(patfile)
			}
			FNR == 1 { fn = "(file-scope)" }
			{
				if ($0 ~ /^func /) {
					name = $0
					sub(/^func[ \t]+/, "", name)
					sub(/^\([^)]*\)[ \t]*/, "", name)
					sub(/[ \t(\[].*$/, "", name)
					fn = (name == "" ? "(file-scope)" : name)
				} else if ($0 ~ /^\}/) {
					fn = "(file-scope)"
				}
				if ($0 ~ /^[ \t]*(\/\/|\*|\/\*)/) next
				if (index($0, "residency:allow") > 0) next
				for (i = 1; i <= npat; i++)
					if ($0 ~ pregex[i]) print FILENAME "\t" fn "\t" pname[i]
			}
		' |
		sed -e "s|^${repo_root}/||" |
		filter_allowlisted | sort | uniq -c |
		sed -E 's|^[[:space:]]*([0-9]+)[[:space:]]+(.*)$|\2\t\1|' | sort
}

filter_allowlisted() {
	local line rel
	while IFS= read -r line; do
		rel=${line%%$'\t'*}
		is_allowlisted "$rel" || printf '%s\n' "$line"
	done
}

if ! load_patterns; then
	exit 1
fi

if ((self_test)); then
	run_self_test || exit 1
	exit 0
fi

for dir in "${scan_dirs[@]}"; do
	if [[ ! -d "$repo_root/$dir" ]]; then
		note "scan directory $dir is missing under $repo_root; the guard cannot evaluate"
		failed=1
	fi
done
if ((failed)); then
	exit 1
fi

current=$(census)

if ((emit_baseline)); then
	printf '%s\n' "# residency-boundary baseline: path <TAB> function <TAB> pattern <TAB> count."
	printf '%s\n' "# SHRINK ONLY. Regenerate when a migration slice RETIRES sites; growth is a"
	printf '%s\n' "# new blind reader and the check refuses it. See scripts/check-residency-boundary.sh."
	printf '%s\n' "$current"
	exit 0
fi

if [[ -z "$current" ]]; then
	note "the census found no enumeration site at all across ${scan_dirs[*]}; the guard is evaluating nothing"
	exit 1
fi
if [[ ! -r "$baseline_file" ]]; then
	note "baseline $baseline_file is missing or unreadable"
	exit 1
fi

declare -A baseline_count=()
declare -A current_count=()

# `ast:` rows belong to the OTHER half of the guard (TestResidencyResolverBoundary,
# scripts/residency_boundary_test.go), which shares this baseline file so there is
# one ratchet rather than two that can disagree. This half ignores them.
while IFS=$'\t' read -r path fn pattern count; do
	[[ -z "${path:-}" || "${path:0:1}" == "#" ]] && continue
	[[ "$pattern" == ast:* ]] && continue
	baseline_count["$path	$fn	$pattern"]=$count
done <"$baseline_file"

if ((${#baseline_count[@]} == 0)); then
	note "baseline $baseline_file declares no site; the ratchet has no denominator"
	exit 1
fi

while IFS=$'\t' read -r path fn pattern count; do
	[[ -z "${path:-}" ]] && continue
	current_count["$path	$fn	$pattern"]=$count
done <<<"$current"

for key in "${!current_count[@]}"; do
	have=${current_count[$key]}
	want=${baseline_count[$key]:-0}
	if ((have > want)); then
		printf 'RESIDENCY-BOUNDARY: %s: %d sites, baseline %d — a NEW store-enumeration site. Consume internal/storeref (Plan/ResolveOwner/Union) or annotate the line with `// residency:allow <reason>`.\n' "${key//	/ }" "$have" "$want"
		failed=1
	fi
done

for key in "${!baseline_count[@]}"; do
	want=${baseline_count[$key]}
	have=${current_count[$key]:-0}
	if ((have < want)); then
		printf 'RESIDENCY-BOUNDARY: %s: %d sites, baseline %d — the baseline must SHRINK in the same commit that retires a site. Run `scripts/check-residency-boundary.sh --emit-baseline > scripts/residency-boundary-baseline.txt`.\n' "${key//	/ }" "$have" "$want"
		failed=1
	fi
done

if ((failed)); then
	echo "---"
	echo "The residency lookup contract lives in internal/storeref. A store list assembled"
	echo "anywhere else re-derives the identity gate, the leg order, the dedupe rule and"
	echo "the fail-loud policy — and every restatement is a chance to get one clause wrong."
	exit 1
fi

exit 0
