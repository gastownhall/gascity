package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// reviewerShapedAgent mirrors the live packs/actual/reviewer/pack.toml agent
// shape that produced the ga-ut74jh incident: min=0/max=1, no scale_check, and
// a narrowing label the upstream stage must add. The incident itself was
// produced by a raw work_query string reproducing this shape; ga-0av489
// replaces that with the sanctioned RouteLabel mechanism, which is what
// ga-atvk13 mechanically converts the live pack.toml to.
func reviewerShapedAgent() *Agent {
	return &Agent{
		Name:       "reviewer",
		Dir:        "cairn",
		RouteLabel: []string{"needs-claude-review"},
	}
}

// TestPoolDemandKeysOnRoutedToNotAssignee is the DISPROOF of the hypothesis
// recorded on ga-ut74jh ("scale-check keys off 'assignee' rather than
// 'routed_to', so routed needs-review demand does not count toward wanting a
// session"). The default pool-demand predicate keys on gc.routed_to and
// explicitly requires an EMPTY assignee, so routed work with no assignee does
// count as demand. Any future change that makes demand assignee-keyed should
// fail here rather than silently validating the wrong root cause.
func TestPoolDemandKeysOnRoutedToNotAssignee(t *testing.T) {
	demand := reviewerShapedAgent().EffectivePoolDemandQuery()

	if !strings.Contains(demand, beadmeta.RoutedToMetadataKey) {
		t.Errorf("EffectivePoolDemandQuery() must key on %s; got %q", beadmeta.RoutedToMetadataKey, demand)
	}
	if !strings.Contains(demand, "--unassigned") {
		t.Errorf("EffectivePoolDemandQuery() must require an empty assignee (--unassigned); got %q", demand)
	}
	// The demand predicate must not select BY a specific assignee — that is the
	// disproven hypothesis. --unassigned is a presence check, not an identity match.
	if strings.Contains(demand, "--assignee") {
		t.Errorf("EffectivePoolDemandQuery() must not filter by a specific --assignee; got %q", demand)
	}
}

// TestNarrowedWorkQueryNarrowsPoolDemand is the regression test for
// ga-ut74jh. Before ga-0av489, queryWork overrode on Agent.WorkQuery while
// queryPoolDemand overrode on Agent.ScaleCheck (workquery.go queryTable), so
// an agent that narrowed its intake with a custom work_query but set no
// matching scale_check kept the label-agnostic default demand predicate.
//
// The reconciler then counted routed beads the worker's work_query could
// never claim: it wakes a session, the session's hook comes back empty, the
// session idles out, and the still-present demand wakes it again. Live
// evidence in the ga-ut74jh incident was 9 session.woke / 5
// session.idle_killed events for cairn/reviewer with zero work claimed,
// because every bead routed to it carried `needs-review` while its
// work_query required `needs-claude-review`.
//
// Demand and claim must agree: if intake is narrowed by label, the demand
// predicate must apply the same narrowing (or the mismatch must be rejected at
// config-load / doctor time). ga-0av489's Agent.RouteLabel closes this
// structurally by feeding both EffectiveWorkQuery's routed-pool tier and
// EffectivePoolDemandQuery from the same field, so a RouteLabel-narrowed
// agent's claim and demand predicates cannot diverge.
func TestNarrowedWorkQueryNarrowsPoolDemand(t *testing.T) {
	agent := reviewerShapedAgent()
	wq := agent.EffectiveWorkQuery()
	demand := agent.EffectivePoolDemandQuery()

	const narrowingLabel = "needs-claude-review"

	// Precondition: the work query really does narrow intake by label.
	if !strings.Contains(wq, narrowingLabel) {
		t.Fatalf("precondition: EffectiveWorkQuery() should carry the custom narrowing label %q; got %q", narrowingLabel, wq)
	}

	// The defect: demand ignores that narrowing, so it counts unclaimable work.
	if !strings.Contains(demand, narrowingLabel) {
		t.Errorf("ga-ut74jh: work_query narrows intake to %q but EffectivePoolDemandQuery() does not, so the reconciler counts demand the worker cannot claim (wake/idle-kill treadmill).\nwork_query: %s\ndemand:     %s", narrowingLabel, wq, demand)
	}
}
