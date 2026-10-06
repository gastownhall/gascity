package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// asleepNamedAliasHolder builds a configured named-session bead that owns the
// canonical alias for "mayor" and is currently asleep — the exact shape live
// gascity/reviewer bead gm-gjmwz2 held in every one of its asleep samples
// (state=asleep AND alias=gascity/reviewer, 4/4 samples between 16:04 and
// 19:59 on 2026-07-24).
func asleepNamedAliasHolder() beads.Bead {
	return beads.Bead{
		ID:     "sess-asleep",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"session_name":               "mayor",
			"template":                   "mayor",
			"alias":                      "mayor",
			"session_origin":             "named",
			"state":                      "asleep",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "on_demand",
		},
	}
}

// TestCanonicalSingletonAliasHeldTemplates_AsleepNamedHolderStillHoldsAlias is
// the missing case in TestCanonicalSingletonAliasHeldTemplates_ExcludesFailedCreateHolder.
//
// That test enumerates the four categories that genuinely RELEASE the canonical
// alias — closed and drained (retire path), pool-managed (never held it), and
// failed-create (failedCreateIdentityReleased in names.go). Each has an explicit
// release mechanism. Sleeping has none: an asleep named session keeps its alias
// and reclaims it on wake.
//
// canonicalSingletonAliasHeldTemplates nonetheless skips asleep holders
// (pool_desired_state.go:355-357), so the pool sees a free alias, mints an
// ephemeral standby, and that standby immediately parks on
// pool_alias_conflict and is drained — the wake/spawn/drain churn in ga-vcmg58.
func TestCanonicalSingletonAliasHeldTemplates_AsleepNamedHolderStillHoldsAlias(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("mayor", "", intPtr(1), 0)}, // canonical singleton
	}

	held := canonicalSingletonAliasHeldTemplates(cfg, sessionInfosFromBeads([]beads.Bead{asleepNamedAliasHolder()}))
	if _, ok := held["mayor"]; !ok {
		t.Fatalf("asleep named holder still owns the canonical alias (sleeping has no release path, "+
			"unlike closed/drained/pool-managed/failed-create) and must mark mayor held; got %v", held)
	}
}

// asleepNamedAliasHolderWithDivergentIdentity mirrors asleepNamedAliasHolder
// but for a named session whose configured identity differs from its backing
// template ("primary" bound to template "worker") — the shape
// TestReconcileSessionBeads_OnDemandNamedSessionWakesFromSingletonPoolDemandWithoutNamedDemand
// exercises end-to-end. build_desired_state.go sets a named session bead's
// Alias to its identity, not its backing template, so the plain
// alias==template comparison in canonicalSingletonAliasHeldTemplates can
// never match this shape; only the Template-based named-session fallback can.
func asleepNamedAliasHolderWithDivergentIdentity() beads.Bead {
	return beads.Bead{
		ID:     "sess-asleep-divergent",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"session_name":               "primary",
			"template":                   "worker",
			"alias":                      "primary",
			"session_origin":             "named",
			"state":                      "asleep",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "primary",
			namedSessionModeMetadata:     "on_demand",
		},
	}
}

// TestCanonicalSingletonAliasHeldTemplates_AsleepNamedHolderIdentityDiffersFromTemplate
// isolates the Template-based fallback match at the canonicalSingletonAliasHeldTemplates
// unit level (not just end-to-end through the reconciler): a named session's
// Alias carries its identity ("primary"), never its backing template
// ("worker"), so the plain Alias==template comparison can never mark the
// template held for this shape — only the isNamedSessionInfo/Template match
// does. Without it, a canonical singleton whose sole named occupant has a
// distinct identity would look permanently free and take a redundant standby
// every reconcile tick.
func TestCanonicalSingletonAliasHeldTemplates_AsleepNamedHolderIdentityDiffersFromTemplate(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("worker", "", intPtr(1), 0)}, // canonical singleton
	}

	held := canonicalSingletonAliasHeldTemplates(cfg, sessionInfosFromBeads([]beads.Bead{asleepNamedAliasHolderWithDivergentIdentity()}))
	if _, ok := held["worker"]; !ok {
		t.Fatalf("named holder with identity %q != template %q must still mark the template held via the "+
			"Template-based fallback match, not just Alias; got %v", "primary", "worker", held)
	}
}

// TestComputePoolDesiredStates_AsleepNamedHolderSuppressesRedundantStandby is
// the end-to-end consequence: routed demand arriving while the named singleton
// sleeps must wake that holder, not mint a second session it can never hand the
// alias to.
//
// Live trace (gascity/reviewer, 2026-07-24, trigger bead ga-z3bhzw):
//
//	18:10:38  pool mints wisp gm-pgn1w      (named holder gm-gjmwz2 asleep since 16:04:39)
//	18:11:00  state=active
//	18:11:20  pool_alias_conflict=gascity/reviewer, count=1   <- born dead
//	18:11:51  pool_alias_conflict_count=3
//	18:13:17  state=drained, closed
//	18:13:38  gm-gjmwz2 wakes and does the work anyway
//
// Net effect: one full worktree setup + agent boot burned per routed bead that
// arrives while the singleton sleeps.
func TestComputePoolDesiredStates_AsleepNamedHolderSuppressesRedundantStandby(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{poolAgent("mayor", "", intPtr(1), 0)},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "on_demand"}},
	}

	// One unit of routed demand, exactly as the default routed-work probe
	// reports it for an on_demand named-backing template
	// (build_desired_state.go:469-471).
	result := ComputePoolDesiredStates(
		cfg,
		nil,
		sessionInfosFromBeads([]beads.Bead{asleepNamedAliasHolder()}),
		map[string]int{"mayor": 1},
	)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Fatalf("pool requests = %d, want 0 — the asleep named holder owns the canonical alias, "+
			"so a pool standby can never acquire it and is drained after parking on "+
			"pool_alias_conflict (ga-vcmg58). Routed demand must wake the holder instead.", total)
	}
}
