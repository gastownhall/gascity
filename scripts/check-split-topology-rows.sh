#!/usr/bin/env bash
# check-split-topology-rows.sh
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
#   Rule A: every invariant subtest — t.Run("I<n>...") — must invoke
#           forEachTopology or forEachTopologyWithRig.
#   Rule B: the conformance file must not call newSplitEnv* directly. All env
#           construction goes through the forEachTopology helpers, which is
#           where the two-row fan-out lives, so no invariant can pin itself to
#           one topology.
#
# Rule C is the non-empty denominator, the lesson check-routed-test-rows.sh
# learned the hard way: a rename of the t.Run naming convention would drop the
# scan to zero invariants and let Rules A and B pass vacuously, silently
# disabling the guard. Finding zero invariants is therefore a failure.
#
# Exits non-zero with each violation printed. Passes silently when the suite is
# fully two-topology. Cheap + static: wired into `make check`.

set -euo pipefail

repo_root=$(cd "$(dirname "$0")/.." && pwd)
suite_rel="cmd/gc/split_topology_conformance_test.go"
suite="$repo_root/$suite_rel"

if [[ ! -f "$suite" ]]; then
    echo "check-split-topology-rows: suite not found: $suite_rel" >&2
    exit 1
fi

violations=0
invariants=0

# Rule A: every invariant subtest routes through a forEachTopology helper.
while IFS= read -r line; do
    lineno=${line%%:*}
    body=${line#*:}
    invariants=$((invariants + 1))
    if [[ "$body" != *forEachTopology* ]]; then
        echo "ROW-GUARD: $suite_rel:$lineno invariant subtest does not run both topologies (missing forEachTopology): ${body#	}"
        violations=$((violations + 1))
    fi
done < <(grep -nE 't\.Run\("I[0-9]' "$suite" || true)

# Rule B: no direct env construction in the suite — it must flow through the
# forEachTopology helpers.
while IFS= read -r line; do
    lineno=${line%%:*}
    body=${line#*:}
    echo "ROW-GUARD: $suite_rel:$lineno direct newSplitEnv bypasses forEachTopology (pins one topology): ${body#	}"
    violations=$((violations + 1))
done < <(grep -nE 'newSplitEnv[A-Za-z]*\(' "$suite" || true)

# Rule C: the guard must be policing something.
if (( invariants == 0 )); then
    echo "ROW-GUARD: $suite_rel declares no t.Run(\"I<n>...\") invariants; the two-topology guard is evaluating nothing." >&2
    exit 1
fi

if (( violations > 0 )); then
    echo "---"
    echo "Split-topology row violations: $violations (over $invariants invariants)"
    echo "Every invariant in TestSplitTopologyConformance must run on BOTH the single-store"
    echo "and split topologies via forEachTopology/forEachTopologyWithRig. An invariant that"
    echo "cannot be expressed on main yet must still route through them and t.Skip with the"
    echo "named reason, so the gap is stated rather than hidden."
    exit 1
fi

exit 0
