package qualification_test

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// TestQ1DefaultCityResolvesOneReservedBindingWithNoProvider is the load-bearing
// Q1 claim about the resolution itself: a default city's storage plan resolves
// against an EMPTY frozen provider registry.
//
// An empty registry cannot answer a single Lookup, so a plan that resolves
// against it has performed none. That is a stronger statement than reading the
// assignment map back: it proves the default path needs no provider bundle, no
// provider ID, and no provider-owned configuration — nothing to register, and
// nothing that could carry a private dependency or a credential into an OSS
// build.
//
// The control is the same registry with an explicit binding: it must fail. A
// resolver that skipped provider resolution altogether would pass the first leg
// and fail this one.
func TestQ1DefaultCityResolvesOneReservedBindingWithNoProvider(t *testing.T) {
	city := q1LoadCityFrom(t, map[string]string{"city.toml": q1MinimalNoStorageCity})
	registry := q1EmptyFrozenRegistry(t)
	pins := q1DefaultPins(q1Pin(storebinding.HQScope(), "gc", "hq"))

	plan, err := storebinding.ResolveStoragePlan(registry, city.EffectiveStorage(), pins)
	if err != nil {
		t.Fatalf("resolving a default city's storage plan against an empty registry: %v", err)
	}

	if bindings := plan.Bindings(); len(bindings) != 0 {
		t.Errorf("the default plan carries %d explicit bindings, want 0", len(bindings))
	}
	for _, class := range coordclass.Classes() {
		binding, assigned := plan.BindingFor(class)
		if !assigned {
			t.Errorf("class %s has no binding assignment", class)
			continue
		}
		if binding != storebinding.ReservedWorkBinding {
			t.Errorf("class %s resolved to binding %q, want the reserved %q", class, binding, storebinding.ReservedWorkBinding)
		}
	}
	if got, want := len(plan.Assignments()), len(coordclass.Classes()); got != want {
		t.Errorf("the plan carries %d class assignments, want %d", got, want)
	}

	open := plan.OpenProgram()
	if len(open) != 1 {
		t.Fatalf("the default plan opens %d bindings, want exactly 1 (the reserved work binding)", len(open))
	}
	step := open[0]
	if step.Binding != storebinding.ReservedWorkBinding || !step.Reserved {
		t.Errorf("the single open step is binding %q (reserved=%t), want the reserved %q", step.Binding, step.Reserved, storebinding.ReservedWorkBinding)
	}
	if step.Provider != nil {
		t.Errorf("the reserved open step carries provider %T; the reserved binding is synthesized, never provider-backed", step.Provider)
	}
	if !step.PinnedWork {
		t.Error("the reserved open step does not own the pinned Work topology")
	}
	for _, class := range coordclass.Classes() {
		if !step.AssignedClasses.Has(class) {
			t.Errorf("the reserved open step does not serve class %s", class)
		}
	}

	closes := plan.CloseProgram()
	if len(closes) != len(open) {
		t.Fatalf("the plan closes %d bindings for %d opened", len(closes), len(open))
	}
	if closes[0].Binding != step.Binding || !closes[0].Reserved {
		t.Errorf("close program head is %q (reserved=%t), want the reverse of the open program", closes[0].Binding, closes[0].Reserved)
	}

	if inspections := plan.InspectionProgram(); len(inspections) != 0 {
		t.Errorf("the default plan schedules %d provider inspections, want 0; a default city performs no provider I/O and no migration", len(inspections))
	}

	if got := plan.NudgeQueueAuthority(); got != storebinding.NudgeQueueRetainedLegacy {
		t.Errorf("nudge queue authority is %s, want %s; a default city keeps the deployed queue authority in place",
			got, storebinding.NudgeQueueRetainedLegacy)
	}

	// Messaging still consumes the Sessions directory at composition. Orders
	// and Graph share the reserved binding, so no orders-graph step is planned:
	// the graph handle Orders binds is the same one it is already served by.
	deferred := plan.DeferredBinds()
	if len(deferred) != 1 {
		t.Fatalf("the default plan schedules %d deferred binds, want exactly the messaging-sessions bind: %+v", len(deferred), deferred)
	}
	if deferred[0].Kind != storebinding.DeferredBindMessagingSessions {
		t.Errorf("the single deferred bind is %s, want %s", deferred[0].Kind, storebinding.DeferredBindMessagingSessions)
	}
	if deferred[0].Consumer != storebinding.ReservedWorkBinding || deferred[0].Supplier != storebinding.ReservedWorkBinding {
		t.Errorf("the messaging-sessions bind runs %q -> %q, want both on the reserved binding",
			deferred[0].Consumer, deferred[0].Supplier)
	}

	t.Run("an explicit binding cannot resolve without a registered provider", func(t *testing.T) {
		relocated := city.EffectiveStorage()
		relocated.Classes.Graph = "infra"
		relocated.Classes.Sessions = "infra"
		relocated.Classes.Messaging = "infra"
		relocated.Classes.Orders = "infra"
		relocated.Classes.Nudges = "infra"
		relocated.Bindings = map[string]config.StorageBindingConfig{
			"infra": {Provider: config.StorageProviderSQLiteBeads, Path: config.DefaultSQLiteStoragePath},
		}
		// The error has to be the REGISTRY refusing the lookup, not a later
		// consistency check: only an unknown-provider failure proves the
		// default path's success came from performing no lookup at all.
		_, err := storebinding.ResolveStoragePlan(registry, relocated, pins)
		if !errors.Is(err, storebinding.ErrUnknownProvider) {
			t.Fatalf("an explicit binding resolved against an empty registry with %v, want a %v; the default-path assertions above prove nothing about provider resolution",
				err, storebinding.ErrUnknownProvider)
		}
	})
}

// TestQ1DefaultPlanEnumeratesHQAndEveryRigWorkspace covers the enumeration rows
// the enumeration surface names, over a default city's pinned topology: HQ plus several
// rig workspaces, config order and lexical order as independent enumerations,
// suspended rigs carried rather than dropped, and the shipped typed error for a
// scope the city does not have.
//
// The rig names are deliberately in non-lexical config order, so the two
// enumerations disagree; if they agreed, either could be silently serving the
// other's callers.
func TestQ1DefaultPlanEnumeratesHQAndEveryRigWorkspace(t *testing.T) {
	pins := q1DefaultPins(
		q1Pin(storebinding.HQScope(), "gc", "hq"),
		q1Pin(storebinding.RigScope("zeta"), "gz", "zeta"),
		q1Suspended(q1Pin(storebinding.RigScope("alpha"), "ga", "alpha")),
		q1Pin(storebinding.RigScope("mu"), "gm", "mu"),
	)
	plan, err := storebinding.ResolveStoragePlan(q1EmptyFrozenRegistry(t), q1DefaultStorage(), pins)
	if err != nil {
		t.Fatalf("resolving a multi-rig default city: %v", err)
	}
	work := plan.WorkPlan()

	if !work.Present() {
		t.Fatal("the reserved binding planned no Work topology")
	}
	if work.HQ().Scope != storebinding.HQScope() {
		t.Errorf("pinned HQ scope is %s, want HQ", work.HQ().Scope)
	}

	if got, want := q1PinnedScopeNames(work.RigsInConfigOrder()), "rig:zeta,rig:alpha,rig:mu"; got != want {
		t.Errorf("config-order rigs = %s, want %s", got, want)
	}
	if got, want := q1PinnedScopeNames(work.RigsInLexicalOrder()), "rig:alpha,rig:mu,rig:zeta"; got != want {
		t.Errorf("lexical-order rigs = %s, want %s", got, want)
	}
	if got, want := q1PinnedScopeNames(work.All()), "hq,rig:alpha,rig:mu,rig:zeta"; got != want {
		t.Errorf("All() = %s, want HQ followed by the lexical rigs", got)
	}

	// Suspension is carried as a fact on the scope, not applied as a filter:
	// each consumer keeps its own inclusion rule, so a plan that dropped the
	// suspended rig would silently remove it from every consumer at once.
	suspended := map[string]bool{}
	for _, rig := range work.RigsInConfigOrder() {
		name, _ := rig.Scope.Rig()
		suspended[name] = rig.Suspended
	}
	if want := map[string]bool{"zeta": false, "alpha": true, "mu": false}; !q1SameSuspension(suspended, want) {
		t.Errorf("pinned suspension = %v, want %v", suspended, want)
	}

	for index, rig := range work.RigsInConfigOrder() {
		if rig.ConfigRank != index+1 {
			t.Errorf("rig %s has config rank %d, want %d (HQ is rank 0)", rig.Scope, rig.ConfigRank, index+1)
		}
	}

	if _, err := work.ForScope(storebinding.RigScope("absent")); !errors.Is(err, storebinding.ErrWorkScopeNotFound) {
		t.Errorf("ForScope on an unconfigured rig returned %v, want a %v", err, storebinding.ErrWorkScopeNotFound)
	}
	var notFound *storebinding.WorkScopeNotFoundError
	if _, err := work.ForScope(storebinding.RigScope("absent")); !errors.As(err, &notFound) {
		t.Errorf("ForScope on an unconfigured rig returned %T, want *storebinding.WorkScopeNotFoundError", err)
	}
}

// TestQ1DefaultPlanGroupsSharedWorkspacesOnce is the first half of the pinning
// contract. A city whose
// rigs share one physical ledger (the unified topology) must still expose every
// semantic scope, while the physical grouping — the unit that gets opened,
// aggregated, and closed — collapses to one entry per distinct ledger.
//
// The scoped control is the same city with distinct ledgers: it must group
// once per scope. Without it, a grouping that always returned one entry would
// pass, and shutdown would close a ledger it never opened.
func TestQ1DefaultPlanGroupsSharedWorkspacesOnce(t *testing.T) {
	t.Run("shared", func(t *testing.T) {
		plan := q1MustResolve(t, q1DefaultPins(
			q1Pin(storebinding.HQScope(), "gc", "shared-ledger"),
			q1Pin(storebinding.RigScope("alpha"), "ga", "shared-ledger"),
			q1Pin(storebinding.RigScope("beta"), "gb", "shared-ledger"),
		))
		groups := plan.WorkPlan().Physical()
		if len(groups) != 1 {
			t.Fatalf("three scopes on one ledger grouped into %d physical workspaces, want 1", len(groups))
		}
		if got, want := q1ScopeNames(groups[0].Scopes), "hq,rig:alpha,rig:beta"; got != want {
			t.Errorf("the shared physical workspace retains scopes %s, want %s", got, want)
		}
		participants, err := plan.WorkParticipants()
		if err != nil {
			t.Fatalf("grouping work participants: %v", err)
		}
		if len(participants) != 1 {
			t.Fatalf("three scopes on one ledger produced %d migration participants, want 1", len(participants))
		}
		if len(participants[0].Members) != 3 {
			t.Errorf("the single participant carries %d members, want 3; a shared ledger opens once but loses no semantic scope", len(participants[0].Members))
		}
		if closes := plan.CloseProgram(); len(closes) != 1 || len(closes[0].PinnedWork) != 1 {
			t.Errorf("the close program closes %d bindings over %d physical workspaces, want 1 and 1", len(closes), len(closes[0].PinnedWork))
		}
	})

	t.Run("scoped", func(t *testing.T) {
		plan := q1MustResolve(t, q1DefaultPins(
			q1Pin(storebinding.HQScope(), "gc", "hq"),
			q1Pin(storebinding.RigScope("alpha"), "ga", "alpha"),
			q1Pin(storebinding.RigScope("beta"), "gb", "beta"),
		))
		groups := plan.WorkPlan().Physical()
		if len(groups) != 3 {
			t.Fatalf("three scopes on three ledgers grouped into %d physical workspaces, want 3; the shared case above is only evidence if distinct ledgers stay distinct", len(groups))
		}
		participants, err := plan.WorkParticipants()
		if err != nil {
			t.Fatalf("grouping work participants: %v", err)
		}
		if len(participants) != 3 {
			t.Fatalf("three distinct ledgers produced %d migration participants, want 3", len(participants))
		}
	})
}

// TestQ1PinnedBootstrapIdentitiesDoNotFollowMutableMetadata is its second
// half. Workspace metadata observed at startup is mutable; the recorded pin is
// not. When they disagree the plan must block rather than adopt the observation
// — a refreshed pin would silently re-point a class at whatever the metadata
// happens to say, which is a migration performed by accident.
//
// The agreeing observation is the control: drift detection that rejected every
// observation would pass the first leg on its own.
func TestQ1PinnedBootstrapIdentitiesDoNotFollowMutableMetadata(t *testing.T) {
	recorded := q1DefaultPins(
		q1Pin(storebinding.HQScope(), "gc", "hq-ledger"),
		q1Pin(storebinding.RigScope("alpha"), "ga", "alpha-ledger"),
	)
	recorded.Recorded = true

	agreeing := recorded
	agreeing.Observed = []storebinding.WorkScopePin{
		q1Pin(storebinding.HQScope(), "gc", "hq-ledger"),
		q1Pin(storebinding.RigScope("alpha"), "ga", "alpha-ledger"),
	}
	if _, err := storebinding.ResolveStoragePlan(q1EmptyFrozenRegistry(t), q1DefaultStorage(), agreeing); err != nil {
		t.Fatalf("an observation agreeing with the recorded pins was rejected: %v", err)
	}

	drifted := recorded
	drifted.Observed = []storebinding.WorkScopePin{
		q1Pin(storebinding.HQScope(), "gc", "hq-ledger"),
		q1Pin(storebinding.RigScope("alpha"), "ga", "alpha-ledger-moved"),
	}
	if _, err := storebinding.ResolveStoragePlan(q1EmptyFrozenRegistry(t), q1DefaultStorage(), drifted); !errors.Is(err, storebinding.ErrWorkPinDrift) {
		t.Fatalf("a drifted physical identity resolved with %v, want a %v; the recorded pin must win", err, storebinding.ErrWorkPinDrift)
	}

	renamed := recorded
	renamed.Observed = []storebinding.WorkScopePin{
		q1Pin(storebinding.HQScope(), "gc", "hq-ledger"),
		q1Pin(storebinding.RigScope("renamed"), "ga", "alpha-ledger"),
	}
	if _, err := storebinding.ResolveStoragePlan(q1EmptyFrozenRegistry(t), q1DefaultStorage(), renamed); !errors.Is(err, storebinding.ErrWorkPinDrift) {
		t.Fatalf("an observed scope the pins do not name resolved with %v, want a %v", err, storebinding.ErrWorkPinDrift)
	}
}

// TestQ1DefaultPlanSelectsWorkspacesByExactPrefix covers the pinned half of
// by-ID selection: an ID whose prefix matches exactly one pinned workspace
// selects it, and an ID matching none reports "no prefix match" rather than an
// error — because the residence probe that follows needs the live stores the
// plan deliberately does not hold.
func TestQ1DefaultPlanSelectsWorkspacesByExactPrefix(t *testing.T) {
	plan := q1MustResolve(t, q1DefaultPins(
		q1Pin(storebinding.HQScope(), "gc", "hq"),
		q1Pin(storebinding.RigScope("alpha"), "ga", "alpha"),
	))
	work := plan.WorkPlan()

	for id, want := range map[string]storebinding.WorkScope{
		"gc-1a2b": storebinding.HQScope(),
		"ga-9z8y": storebinding.RigScope("alpha"),
	} {
		scope, matched, err := work.PrefixScopeForID(id)
		if err != nil {
			t.Fatalf("prefix-selecting %s: %v", id, err)
		}
		if !matched {
			t.Fatalf("%s matched no pinned prefix", id)
		}
		if scope != want {
			t.Errorf("%s selected %s, want %s", id, scope, want)
		}
	}

	scope, matched, err := work.PrefixScopeForID("gx-0000")
	if err != nil {
		t.Fatalf("prefix-selecting an unknown prefix returned an error: %v", err)
	}
	if matched {
		t.Errorf("an unknown prefix selected %s; the residence probe must be allowed to run", scope)
	}
}

// TestQ1DefaultPlanRejectsUnselectableWorkspacePrefixes pins the other half of
// by-ID selection at plan time: two workspaces whose prefixes cannot pick out a
// unique scope are rejected before anything opens, so no ID is ever ambiguous
// at read time.
func TestQ1DefaultPlanRejectsUnselectableWorkspacePrefixes(t *testing.T) {
	_, err := storebinding.ResolveStoragePlan(q1EmptyFrozenRegistry(t), q1DefaultStorage(), q1DefaultPins(
		q1Pin(storebinding.HQScope(), "gc", "hq"),
		q1Pin(storebinding.RigScope("alpha"), "gc", "alpha"),
	))
	if !errors.Is(err, storebinding.ErrInvalidWorkPin) {
		t.Fatalf("two workspaces sharing prefix %q resolved with %v, want a %v", "gc", err, storebinding.ErrInvalidWorkPin)
	}
}

// q1DefaultStorage is the effective storage configuration of a city that
// authors no [storage] section: the six-class map every default city resolves
// to. It is read from config rather than written out here, so a change to the
// default map moves this suite with it instead of drifting from it.
func q1DefaultStorage() config.StorageConfig {
	var city config.City
	return city.EffectiveStorage()
}

// q1MustResolve resolves a default city's plan for pins, failing the test on
// any error.
func q1MustResolve(t *testing.T, pins storebinding.WorkPinInputs) *storebinding.StoragePlan {
	t.Helper()
	plan, err := storebinding.ResolveStoragePlan(q1EmptyFrozenRegistry(t), q1DefaultStorage(), pins)
	if err != nil {
		t.Fatalf("resolving a default city's storage plan: %v", err)
	}
	return plan
}

// q1SameSuspension compares two rig-name to suspended maps.
func q1SameSuspension(got, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for name, suspended := range want {
		if got[name] != suspended {
			return false
		}
	}
	return true
}
