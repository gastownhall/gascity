package config

import (
	"strings"
	"testing"
)

// liveInvestigatorWorkQuery is copied verbatim from
// packs/actual/investigator/pack.toml:30 in the gc-management city pack. It is
// the shape 21 of the live packs use: a single label-gated `bd ready`.
const liveInvestigatorWorkQuery = `gc bd --rig {{.Rig}} ready --label=needs-investigation --exclude-label hold:mayor,hold:external --json 2>/dev/null`

// TestRawWorkQueryOverrideCollapsesAllDiscoveryTiers is the ga-f57vc7 proof.
//
// queryTable (workquery.go:284-303) wires FOUR query kinds — queryWork,
// queryAssignedInProgress, queryAssignedReady, queryRoutedPool — to the SAME
// override slot, Agent.WorkQuery. effectiveQuery returns a non-empty override
// verbatim. So setting work_query in a pack.toml does not "narrow the label
// the agent claims on", which is how every one of the 21 live overrides reads;
// it REPLACES the entire multi-tier discovery contract with one label query.
//
// The two consequences this test pins:
//
//  1. The routed-pool tier disappears from the claim path. A bead carrying
//     gc.routed_to=<me> and no label is unreachable by the override, while
//     EffectivePoolDemandQuery still counts it as demand (it is label-agnostic
//     by construction, workquery.go:41-43). Demand without a matching claim is
//     the wake -> empty hook -> idle -> idle_killed treadmill.
//
//  2. Crash recovery disappears too. The assigned-in-progress tier is how an
//     agent re-finds its own in_progress bead after a crash; the override
//     answers that question with the same label query, which filters on label
//     and not on assignee.
func TestRawWorkQueryOverrideCollapsesAllDiscoveryTiers(t *testing.T) {
	overridden := &Agent{Name: "investigator", WorkQuery: liveInvestigatorWorkQuery}

	// Every tier accessor hands back the one override string.
	for _, tc := range []struct {
		tier string
		got  string
	}{
		{"work", overridden.EffectiveWorkQuery()},
		{"assigned_in_progress (crash recovery)", overridden.EffectiveAssignedInProgressQuery()},
		{"routed_pool", overridden.EffectiveRoutedPoolQuery()},
	} {
		if tc.got != liveInvestigatorWorkQuery {
			t.Errorf("tier %s: expected the raw override verbatim, got:\n%s", tc.tier, tc.got)
		}
	}

	// (1) The claim path lost routed-to-me entirely...
	if strings.Contains(overridden.EffectiveRoutedPoolQuery(), "gc.routed_to") {
		t.Errorf("routed_pool tier unexpectedly still filters on gc.routed_to:\n%s",
			overridden.EffectiveRoutedPoolQuery())
	}
	// ...while the demand query the reconciler wakes on still counts it.
	demand := overridden.EffectivePoolDemandQuery()
	if !strings.Contains(demand, "gc.routed_to") {
		t.Fatalf("pool demand query no longer keys on gc.routed_to; this test is stale:\n%s", demand)
	}
	if strings.Contains(demand, "needs-investigation") {
		t.Errorf("pool demand unexpectedly label-narrowed; divergence may already be fixed:\n%s", demand)
	}
	t.Logf("DIVERGENCE — demand counts a routed bead the claim path cannot see.\n"+
		"  demand (wakes the session): %s\n  claim  (what it can take):  %s",
		demand, overridden.EffectiveWorkQuery())

	// (2) Crash recovery no longer filters on assignee.
	if strings.Contains(overridden.EffectiveAssignedInProgressQuery(), "assignee") {
		t.Errorf("assigned-in-progress tier unexpectedly still filters on assignee:\n%s",
			overridden.EffectiveAssignedInProgressQuery())
	}
}

// TestDefaultWorkQueryAlreadyImplementsLabelOrRoutedToMe records the finding
// that reframes the sanctioned fix: the operator-approved
// "label OR routed-to-me" shape is not a new semantic to build. The DEFAULT
// (no-override) work query already ORs the assignee tiers with a routed-pool
// tier keyed on gc.routed_to. What the 21 packs need is to NARROW that default
// by label, not to REPLACE it with a label query.
//
// This is why the fix belongs in ga-0av489's structured Agent.RouteLabel field
// (which the tier builders consume) rather than in per-pack work_query strings,
// and why the pack-author two-query shell union is not a template to copy.
func TestDefaultWorkQueryAlreadyImplementsLabelOrRoutedToMe(t *testing.T) {
	def := &Agent{Name: "investigator"}

	work := def.EffectiveWorkQuery()
	if !strings.Contains(work, "gc.routed_to") {
		t.Errorf("default work query lost its routed-to-me tier:\n%s", work)
	}
	if !strings.Contains(work, "assignee") {
		t.Errorf("default work query lost its assignee (crash-recovery) tier:\n%s", work)
	}

	// The routed tier targets this agent specifically, so widening the claim
	// path to "routed-to-me" cannot pull in a bead routed to somebody else.
	// That is the answer to the bulk-labeling trap: adding a label makes a
	// bead claimable rig-wide, adding routing does not.
	routed := def.EffectiveRoutedPoolQuery()
	if !strings.Contains(routed, "investigator") {
		t.Errorf("routed-pool tier is not scoped to this agent's qualified name:\n%s", routed)
	}
}
