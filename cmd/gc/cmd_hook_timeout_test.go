package main

import (
	"testing"
	"time"
)

// TestWorkQueryTimeoutsAccommodateMultiRoundTripProbe guards the work-query
// timeout budget. The default work-probe (config.Agent.EffectiveWorkQuery)
// issues ~6 sequential bd/store round-trips — three session identifiers across
// the in-progress and ready assigned tiers — before the pool-demand tier that
// finds routed work. On a multi-rig dolt city under concurrent load each
// round-trip costs several seconds, so at the prior 15s (gc hook run) / 30s
// (work-query subprocess) budgets the probe was killed before reaching
// pool-demand and pool operators (gc.run-operator) were starved of work they
// had already been routed. Keep both budgets generous enough to clear the
// realistic loaded cost, and keep the subprocess cap at least as large as the
// managed-hook wrapper so the wrapper never preempts the inner work query.
func TestWorkQueryTimeoutsAccommodateMultiRoundTripProbe(t *testing.T) {
	const minProbeBudget = 30 * time.Second

	if defaultHookRunTimeout < minProbeBudget {
		t.Errorf("defaultHookRunTimeout = %s, want >= %s (multi-round-trip probe budget)", defaultHookRunTimeout, minProbeBudget)
	}
	if hookWorkQueryTimeout < minProbeBudget {
		t.Errorf("hookWorkQueryTimeout = %s, want >= %s (multi-round-trip probe budget)", hookWorkQueryTimeout, minProbeBudget)
	}
	if hookWorkQueryTimeout < defaultHookRunTimeout {
		t.Errorf("hookWorkQueryTimeout (%s) must be >= defaultHookRunTimeout (%s) so the managed-hook wrapper does not preempt the inner work-query subprocess", hookWorkQueryTimeout, defaultHookRunTimeout)
	}
}
