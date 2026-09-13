#!/usr/bin/env bash
# check-split-topology-rows.sh [repo-root]
#
# Drift-prevention lint for the split-store conformance suite
# (cmd/gc/split_topology_conformance_test.go, TestSplitTopologyConformance).
#
# The suite's whole value is that every invariant runs on BOTH topologies:
#
#     single-store  — routes == nil, resolveClassStore collapses every class to
#                     the work store (the legacy, pre-split city)
#     split         — routes relocate all five infrastructure classes to one
#                     binding store (the two-database city under test)
#
# forEachTopology / forEachTopologyWithRig run a t.Run per topology, so an
# invariant routed through them is guarded on both. An invariant that minted its
# own env inline (newSplitEnv(t, true)) would silently cover ONE row, and a
# regression that broke the other topology would sail through — which is exactly
# how the two production bugs this suite exists for got in: a fix that was
# correct on one store arrangement and wrong on the other. This guard forbids
# that shape:
#
#   Rule A: every invariant subtest — t.Run("I<n>...") — must invoke one of the
#           declared fan-out helpers, anywhere in its subtest body. The body is
#           read by brace balance, not by line, so the idiomatic multi-line
#           subtest is accepted and a helper call on a later line still counts.
#   Rule B: a suite file must not call newSplitEnv* directly. All env
#           construction goes through the fan-out helpers, which is where the
#           two-row expansion lives, so no invariant can pin itself to one
#           topology.
#
# Rule C is the non-empty denominator, the lesson check-routed-test-rows.sh
# learned the hard way: a rename of the t.Run naming convention would drop the
# scan to zero invariants and let Rules A and B pass vacuously, silently
# disabling the guard. Finding zero suite files, or zero invariants across them,
# is therefore a failure.
#
# DISCOVERY: the suite is found by content, not by name. Any *_test.go under
# cmd/gc that declares a t.Run("I<n>...") invariant is a suite file, so a second
# conformance file is policed the day it is added rather than the day someone
# remembers to list it here. The fixture file that DEFINES the helpers is
# excluded from Rule B — it is where newSplitEnv legitimately lives.
#
# Exits non-zero with each violation printed. Passes silently when every suite
# file is fully two-topology. Cheap + static: wired into `make check`.

set -euo pipefail

repo_root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
scan_dir="$repo_root/cmd/gc"

# The fan-out helpers a suite file may route an invariant through. Adding a new
# one is a deliberate edit here: a helper that ran a single topology would
# otherwise satisfy Rule A by name alone.
topology_helpers=(
    "forEachTopology("
    "forEachTopologyWithRig("
)

# Rendered for the failure message: "forEachTopology, forEachTopologyWithRig".
helper_list=$(printf '%s, ' "${topology_helpers[@]%(}")
helper_list=${helper_list%, }

if [[ ! -d "$scan_dir" ]]; then
    echo "check-split-topology-rows: no cmd/gc directory under $repo_root" >&2
    exit 1
fi

# An invariant subtest declaration: t.Run("I<n>...".
invariant_re='t\.Run\("I[0-9]'
# Direct env construction, the Rule B shape.
direct_env_re='newSplitEnv[A-Za-z]*\('
# A `grep -n` hit whose source line is a comment. Doc comments quote both
# shapes, and counting them would let a file of prose satisfy Rule C's
# non-empty denominator and would report a documented example as a violation.
comment_hit_re='^[0-9]+:[[:space:]]*(//|\*|/\*)'
# The fixture file that defines the fan-out helpers. It constructs envs by
# design, so Rule B does not apply to it.
fixture_re='func forEachTopology\('

# code_hits FILE REGEX -> `grep -n` hits with comment lines dropped.
code_hits() {
    grep -nE "$2" "$1" 2>/dev/null | grep -vE "$comment_hit_re" || true
}

violations=0
invariants=0
suite_files=0

# subtest_body FILE LINENO -> echoes the subtest body starting at LINENO,
# ending when the braces opened on that line balance again. That is what makes
# Rule A read a multi-line subtest the same way the compiler does; a same-line
# substring match reports the idiomatic form as a violation.
subtest_body() {
    awk -v start="$2" '
        NR < start { next }
        {
            line = $0
            # Strip // comments so a brace inside one does not skew the balance.
            sub(/\/\/.*/, "", line)
            print $0
            n = gsub(/\{/, "{", line)
            m = gsub(/\}/, "}", line)
            depth += n - m
            if (NR > start || n > 0) {
                if (depth <= 0) exit
            }
        }
    ' "$1"
}

while IFS= read -r suite; do
    hits=$(code_hits "$suite" "$invariant_re")
    if [[ -z "$hits" ]]; then
        # Only doc comments mention the convention: not a suite file.
        continue
    fi
    suite_files=$((suite_files + 1))
    suite_rel=${suite#"$repo_root"/}

    # Rule A: every invariant subtest routes through a declared fan-out helper.
    while IFS= read -r line; do
        lineno=${line%%:*}
        decl=${line#*:}
        invariants=$((invariants + 1))
        body=$(subtest_body "$suite" "$lineno")
        matched=0
        for helper in "${topology_helpers[@]}"; do
            if [[ "$body" == *"$helper"* ]]; then
                matched=1
                break
            fi
        done
        if (( matched == 0 )); then
            echo "ROW-GUARD: $suite_rel:$lineno invariant subtest does not run both topologies (its body calls none of: ${helper_list}): ${decl#	}"
            violations=$((violations + 1))
        fi
    done <<< "$hits"

    # Rule B: no direct env construction in a suite file — it must flow through
    # the fan-out helpers. The fixture file that defines them is exempt.
    if grep -qE "$fixture_re" "$suite"; then
        continue
    fi
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        lineno=${line%%:*}
        body=${line#*:}
        echo "ROW-GUARD: $suite_rel:$lineno direct newSplitEnv bypasses the fan-out helpers (pins one topology): ${body#	}"
        violations=$((violations + 1))
    done <<< "$(code_hits "$suite" "$direct_env_re")"
done < <(grep -rlE "$invariant_re" "$scan_dir" --include='*_test.go' | sort)

# Rule C: the guard must be policing something.
if (( suite_files == 0 )); then
    echo "ROW-GUARD: no cmd/gc test file declares a t.Run(\"I<n>...\") invariant; the two-topology guard found no suite to police." >&2
    exit 1
fi
if (( invariants == 0 )); then
    echo "ROW-GUARD: the $suite_files discovered suite file(s) declare no t.Run(\"I<n>...\") invariants; the two-topology guard is evaluating nothing." >&2
    exit 1
fi

if (( violations > 0 )); then
    echo "---"
    echo "Split-topology row violations: $violations (over $invariants invariants in $suite_files file(s))"
    echo "Every invariant in TestSplitTopologyConformance must run on BOTH the single-store"
    echo "and split topologies via forEachTopology/forEachTopologyWithRig. An invariant that"
    echo "cannot be expressed on main yet must still route through them and t.Skip with the"
    echo "named reason, so the gap is stated rather than hidden."
    exit 1
fi

exit 0
