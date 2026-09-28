package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/storeref"
)

const warmClaimText = "Run gc hook --claim --json now; if it returns work, execute the claimed formula immediately."

func warmBindPoolSession() *beads.Bead {
	return &beads.Bead{
		ID:     "s-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name":                    "worker-1",
			"pool_managed":                    "true",
			beadmeta.TriggerBeadIDMetadataKey: "w-1",
		},
	}
}

func alwaysUnclaimed(beads.Bead) bool { return true }
func neverUnclaimed(beads.Bead) bool  { return false }

// countNudges returns how many Nudge calls the fake recorded and the message of
// the last one.
func countNudges(sp *runtime.Fake) (int, string) {
	n, last := 0, ""
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			n++
			last = c.Message
		}
	}
	return n, last
}

// A warm slot with a newly-bound, unclaimed trigger is nudged exactly once with
// the claim text and the marker is persisted; a second pass does not re-nudge
// (marker guard); binding a different trigger fires exactly one more nudge.
func TestDeliverWarmBindClaimNudge_FiresOncePerBinding(t *testing.T) {
	sp := runtime.NewFake()
	// tmux-like default: activity reporting ON. The hook must still fire — proving
	// it is provider-agnostic, not gated on CanReportActivity.
	if !sp.Capabilities().CanReportActivity {
		t.Fatal("precondition: default fake should report activity (tmux-like)")
	}
	session := warmBindPoolSession()
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	// Pass 1: fires once, stamps the marker.
	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)
	if n, last := countNudges(sp); n != 1 || last != warmClaimText {
		t.Fatalf("pass 1: got %d nudges (last=%q), want 1 with claim text", n, last)
	}
	if got := session.Metadata[warmBindNudgedForTriggerKey]; got != "w-1" {
		t.Fatalf("pass 1: in-memory marker = %q, want w-1", got)
	}
	stored, _ := store.Get("s-1")
	if got := stored.Metadata[warmBindNudgedForTriggerKey]; got != "w-1" {
		t.Fatalf("pass 1: persisted marker = %q, want w-1", got)
	}

	// Pass 2: same binding — marker matches, no re-nudge.
	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)
	if n, _ := countNudges(sp); n != 1 {
		t.Fatalf("pass 2: got %d nudges, want still 1 (marker guard)", n)
	}

	// Rebind to a different trigger — fires exactly once more.
	session.Metadata[beadmeta.TriggerBeadIDMetadataKey] = "w-2"
	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)
	if n, last := countNudges(sp); n != 2 || last != warmClaimText {
		t.Fatalf("rebind: got %d nudges (last=%q), want 2 with claim text", n, last)
	}
	if got := session.Metadata[warmBindNudgedForTriggerKey]; got != "w-2" {
		t.Fatalf("rebind: marker = %q, want w-2", got)
	}
}

// The idle-ready gate runs before delivery so the nudge never lands mid-turn.
func TestDeliverWarmBindClaimNudge_WaitsForIdleBeforeDelivering(t *testing.T) {
	sp := runtime.NewFake()
	session := warmBindPoolSession()
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)

	sawWait, sawNudge := false, false
	for _, c := range sp.Calls {
		switch c.Method {
		case "WaitForIdle":
			if c.Name == "worker-1" {
				sawWait = true
			}
		case "Nudge":
			if !sawWait {
				t.Fatal("Nudge delivered before WaitForIdle (mid-turn risk)")
			}
			sawNudge = true
		}
	}
	if !sawWait || !sawNudge {
		t.Fatalf("want both WaitForIdle and Nudge; got wait=%v nudge=%v", sawWait, sawNudge)
	}
}

// A claimed trigger (probe returns false) is never nudged and never marked — the
// churn invariant that keeps the nudge invisible to a working slot.
func TestDeliverWarmBindClaimNudge_SkipsClaimedTrigger(t *testing.T) {
	sp := runtime.NewFake()
	session := warmBindPoolSession()
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, neverUnclaimed)
	if n, _ := countNudges(sp); n != 0 {
		t.Fatalf("claimed trigger: got %d nudges, want 0", n)
	}
	if got := session.Metadata[warmBindNudgedForTriggerKey]; got != "" {
		t.Fatalf("claimed trigger: marker = %q, want empty", got)
	}
}

// A non-pool session is ignored entirely (named sessions carry no claim work).
func TestDeliverWarmBindClaimNudge_SkipsNonPool(t *testing.T) {
	sp := runtime.NewFake()
	session := warmBindPoolSession()
	delete(session.Metadata, "pool_managed")
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)
	if n, _ := countNudges(sp); n != 0 {
		t.Fatalf("non-pool: got %d nudges, want 0", n)
	}
}

// A slot with no bound trigger, no claim text, or a nil probe delivers nothing.
func TestDeliverWarmBindClaimNudge_NoopGuards(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{*warmBindPoolSession()}, nil)

	cases := map[string]func() (*runtime.Fake, *beads.Bead, warmClaimTriggerProbe, string){
		"no bound trigger": func() (*runtime.Fake, *beads.Bead, warmClaimTriggerProbe, string) {
			s := warmBindPoolSession()
			delete(s.Metadata, beadmeta.TriggerBeadIDMetadataKey)
			return runtime.NewFake(), s, alwaysUnclaimed, warmClaimText
		},
		"empty claim text": func() (*runtime.Fake, *beads.Bead, warmClaimTriggerProbe, string) {
			return runtime.NewFake(), warmBindPoolSession(), alwaysUnclaimed, "   "
		},
		"nil probe": func() (*runtime.Fake, *beads.Bead, warmClaimTriggerProbe, string) { //nolint:unparam // the always-nil probe IS this case
			return runtime.NewFake(), warmBindPoolSession(), nil, warmClaimText
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			sp, session, probe, text := build()
			deliverWarmBindClaimNudge(context.Background(), sp, store, session, text, probe)
			if n, _ := countNudges(sp); n != 0 {
				t.Fatalf("%s: got %d nudges, want 0", name, n)
			}
		})
	}
}

// A delivery that fails leaves the marker unset so a later tick retries; the
// unclaimed gate keeps that retry safe.
func TestDeliverWarmBindClaimNudge_NoMarkerOnDeliveryFailure(t *testing.T) {
	sp := runtime.NewFailFake() // every provider op errors, incl. Nudge
	session := warmBindPoolSession()
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)

	if n, _ := countNudges(sp); n != 1 {
		t.Fatalf("delivery failure: want exactly one Nudge attempt, got %d", n)
	}
	if got := session.Metadata[warmBindNudgedForTriggerKey]; got != "" {
		t.Fatalf("delivery failure: in-memory marker = %q, want empty (retry next tick)", got)
	}
	stored, _ := store.Get("s-1")
	if got := stored.Metadata[warmBindNudgedForTriggerKey]; got != "" {
		t.Fatalf("delivery failure: persisted marker = %q, want empty", got)
	}
}

// isUnclaimedTrigger: only an open bead not already assigned to this slot counts.
func TestIsUnclaimedTrigger(t *testing.T) {
	cases := []struct {
		name string
		w    beads.Bead
		want bool
	}{
		{"open unassigned", beads.Bead{Status: "open"}, true},
		{"open assigned elsewhere", beads.Bead{Status: "open", Assignee: "worker-9"}, true},
		{"open assigned to self", beads.Bead{Status: "open", Assignee: "worker-1"}, false},
		{"in_progress", beads.Bead{Status: "in_progress"}, false},
		{"closed", beads.Bead{Status: "closed"}, false},
		{"blocked", beads.Bead{Status: "blocked"}, false},
	}
	for _, tc := range cases {
		if got := isUnclaimedTrigger(tc.w, "worker-1"); got != tc.want {
			t.Errorf("%s: isUnclaimedTrigger = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// warmClaimRuntimeFor builds the controller the warm-bind lane reads through.
// Going through a real CityRuntime rather than a hand-assembled storeref.Topology
// is what makes these tests assert the production wiring: a topology written out
// here would keep passing after the controller stopped building one.
func warmClaimRuntimeFor(cfg *config.City, work beads.Store, rigs map[string]beads.Store, routes *storageRoutes) *CityRuntime {
	return &CityRuntime{
		cfg:                 cfg,
		standaloneCityStore: work,
		standaloneRigStores: rigs,
		storageRoutes:       routes,
	}
}

// warmClaimResolverFor builds the reader the reconciler hands the probe.
func warmClaimResolverFor(cfg *config.City, work beads.Store, rigs map[string]beads.Store, routes *storageRoutes) warmClaimTriggerResolver {
	cr := warmClaimRuntimeFor(cfg, work, rigs, routes)
	return cr.newWarmClaimTriggerResolver(cr.rigBeadStores())
}

// warmClaimProbeFor builds the probe the reconciler installs over that resolver.
func warmClaimProbeFor(cfg *config.City, work beads.Store, rigs map[string]beads.Store, routes *storageRoutes) warmClaimTriggerProbe {
	return buildWarmClaimTriggerProbe(warmClaimResolverFor(cfg, work, rigs, routes), io.Discard)
}

// warmClaimByIDPlan is the by-id plan the warm-bind resolver executes for id.
// Read alongside the probe assertions, it is what keeps a "the city copy won"
// result from also passing on a plan that quietly lost the rig leg.
func warmClaimByIDPlan(t *testing.T, cr *CityRuntime, id string) string {
	t.Helper()
	plan, err := storeref.Plan(storeref.ByID{ID: id}, cr.residencyTopology(cr.rigBeadStores()))
	if err != nil {
		t.Fatalf("Plan(ByID{%s}): %v", id, err)
	}
	return plan.String()
}

// warmBindStampedSession is a pool slot bound to trigger, carrying the
// demand-leg stamp the binder wrote.
func warmBindStampedSession(trigger, storeRef string) beads.Bead {
	return beads.Bead{Metadata: map[string]string{
		"session_name":                          "worker-1",
		beadmeta.TriggerBeadIDMetadataKey:       trigger,
		beadmeta.TriggerBeadStoreRefMetadataKey: storeRef,
	}}
}

// warmBindDemandLegStamps are gc.trigger_bead_store_ref values a live city
// writes for one and the same binding-resident trigger. Each names the demand
// LEG the row was counted under — the group key build_desired_state carried into
// the bind — and none of them is a statement about where the bead lives, so all
// of them must resolve to the same row.
var warmBindDemandLegStamps = []string{"", "city", "city:test-city", "class:gmnos", "rig:alpha"}

// A split city keeps its graph-class step beads in the class binding, which is
// in no work ledger at all. The probe resolves the trigger through the residency
// contract, so every demand-leg stamp answers from the binding.
func TestBuildWarmClaimTriggerProbe_ResolvesBindingResidentTrigger(t *testing.T) {
	binding := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gcg-1", Status: "open"}}, nil)
	work := beads.NewMemStore()
	alpha := beads.NewMemStore()
	probe := warmClaimProbeFor(residencyTestConfig(), work, map[string]beads.Store{"alpha": alpha}, splitRoutes(binding))

	for _, stamp := range warmBindDemandLegStamps {
		if !probe(warmBindStampedSession("gcg-1", stamp)) {
			t.Errorf("stamp %q: want unclaimed=true — the binding holds the row, and no stamp value names a store that does", stamp)
		}
	}

	// The churn invariant still holds on the binding row: the instant the slot
	// claims, the probe goes quiet under every stamp.
	claimed, assignee := "in_progress", "worker-1"
	if err := binding.Update("gcg-1", beads.UpdateOpts{Status: &claimed, Assignee: &assignee}); err != nil {
		t.Fatalf("claiming gcg-1 in the binding: %v", err)
	}
	for _, stamp := range warmBindDemandLegStamps {
		if probe(warmBindStampedSession("gcg-1", stamp)) {
			t.Errorf("stamp %q: a claimed trigger must read unclaimed=false", stamp)
		}
	}
}

// `gc storage migrate` PRESERVES ids, so a migrated city can hold a frozen
// same-id copy of a relocated bead in its work ledger. That relic stays open
// forever and reads as unclaimed; the binding is the authority, so the probe
// answers from it and never from the relic.
func TestBuildWarmClaimTriggerProbe_BindingShadowsRelicCopy(t *testing.T) {
	relic := beads.Bead{ID: "gcg-2", Status: "open"}
	binding := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "gcg-2", Status: "in_progress", Assignee: "worker-1"},
	}, nil)
	probe := warmClaimProbeFor(
		residencyTestConfig(),
		beads.NewMemStoreFrom(0, []beads.Bead{relic}, nil),
		nil,
		splitRoutes(binding),
	)
	if probe(warmBindStampedSession("gcg-2", "city:test-city")) {
		t.Fatal("the work-ledger relic answered: a claimed binding row must shadow the frozen pre-migration copy")
	}

	// Control: the SAME relic on a city that relocates nothing is the only copy
	// there is, and it answers — so the assertion above is the binding winning
	// rather than the probe having gone blind to work.
	legacy := warmClaimProbeFor(
		residencyTestConfig(),
		beads.NewMemStoreFrom(0, []beads.Bead{relic}, nil),
		nil,
		nil,
	)
	if !legacy(warmBindStampedSession("gcg-2", "city:test-city")) {
		t.Fatal("a legacy city lost its own work-ledger trigger")
	}
}

// The legacy answer, unchanged: on a city with no bindings a by-id plan is the
// work ledger plus the rig legs whose configured prefix covers the id, which is
// the store set the pre-resolver parser reached through the stamp.
//
// This is the adapted TestBuildWarmClaimTriggerProbe_ResolvesFromStoreRef. The
// one case that could not survive is the unknown-rig fail-closed: there is no
// hand parser left to feed a bad stamp to, so it is inverted below into the
// assertion that a wrong stamp no longer decides anything.
func TestBuildWarmClaimTriggerProbe_LegacyCityResolvesWorkAndRigLegs(t *testing.T) {
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "c-1", Status: "open"}}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "ra-1", Status: "open"},
		{ID: "ra-2", Status: "open"},
	}, nil)
	probe := warmClaimProbeFor(residencyTestConfig(), cityStore, map[string]beads.Store{"alpha": rigStore}, nil)

	// The rig leg: ra-1 is inside rig alpha's configured prefix and open.
	if !probe(warmBindStampedSession("ra-1", "rig:alpha")) {
		t.Fatal("rig-resident open trigger: want unclaimed=true")
	}
	// Claim it in the rig store → the probe flips.
	claimed := "in_progress"
	if err := rigStore.Update("ra-1", beads.UpdateOpts{Status: &claimed}); err != nil {
		t.Fatalf("claiming ra-1: %v", err)
	}
	if probe(warmBindStampedSession("ra-1", "rig:alpha")) {
		t.Fatal("rig-resident claimed trigger: want unclaimed=false")
	}
	// The work leg: c-1 is outside every rig prefix and lives in the city ledger.
	if !probe(warmBindStampedSession("c-1", "")) {
		t.Fatal("empty-stamp open trigger: want unclaimed=true (city work store)")
	}
	// A stamp naming a store the bead is not in no longer decides anything.
	if !probe(warmBindStampedSession("ra-2", "rig:ghost")) {
		t.Fatal("a stamp naming an unknown store must not suppress a resolvable trigger")
	}
	// A trigger no leg holds is a miss, and a miss never nudges.
	if probe(warmBindStampedSession("missing", "")) {
		t.Fatal("missing trigger bead: want fail-closed unclaimed=false")
	}
	// No bound trigger id → nothing to probe.
	if probe(warmBindStampedSession("", "rig:alpha")) {
		t.Fatal("no trigger id: want unclaimed=false")
	}
}

// A same-id collision between the city work ledger and a rig store — on an id
// INSIDE that rig's configured prefix, so the rig leg is genuinely in the plan —
// resolves to the city copy, because Plan(ByID) probes the work fallback ahead of
// the prefix-gated rig shadows.
//
// This pins the order, which is what makes it a decision rather than an accident.
// It is deliberately the OPPOSITE of TestControlDispatchRigScopePrefersItsOwnStore:
// that lane is HANDED a rig scope and pins the rig's own store first, while a pool
// slot's trigger arrives with no scope at all, so nothing licenses a rig leg to
// lead and the house by-id order (cliByIDOwner, internal/api's by-id resolver)
// applies unchanged. newWarmClaimTriggerResolver's doc records the divergence.
//
// The two ledgers hold disjoint ids today, so the order cannot change a live
// answer; pinning it means a future migration that does mint a co-resident id
// changes THIS test rather than silently flipping every warm-bind nudge decision.
func TestBuildWarmClaimTriggerProbe_SameIDCollisionResolvesToCityCopy(t *testing.T) {
	// ra-1 is inside rig alpha's configured prefix, and both ledgers hold it.
	collide := func(cityStatus, rigStatus string) *CityRuntime {
		return warmClaimRuntimeFor(
			residencyTestConfig(),
			beads.NewMemStoreFrom(0, []beads.Bead{{ID: "ra-1", Status: cityStatus}}, nil),
			map[string]beads.Store{"alpha": beads.NewMemStoreFrom(0, []beads.Bead{{ID: "ra-1", Status: rigStatus}}, nil)},
			nil,
		)
	}
	probeOf := func(cr *CityRuntime) warmClaimTriggerProbe {
		return buildWarmClaimTriggerProbe(cr.newWarmClaimTriggerResolver(cr.rigBeadStores()), io.Discard)
	}

	// Precondition: BOTH ledgers are in the plan and the work leg leads. Without
	// it, "the city copy decided" would also pass on a plan that lost the rig leg.
	const wantPlan = `FirstOwner: ""[WorkFallback,Fatal] > rig:alpha[Shadow,Fatal]`
	if got := warmClaimByIDPlan(t, collide("open", "open"), "ra-1"); got != wantPlan {
		t.Fatalf("by-id plan = %q, want %q", got, wantPlan)
	}

	if probeOf(collide("in_progress", "open"))(warmBindStampedSession("ra-1", "rig:alpha")) {
		t.Error("a claimed city copy must decide the nudge: the work leg is probed before the rig shadow")
	}
	if !probeOf(collide("open", "in_progress"))(warmBindStampedSession("ra-1", "rig:alpha")) {
		t.Error("an open city copy must decide the nudge: the work leg is probed before the rig shadow")
	}

	// And the rig shadow is a live leg, not a decoration: with no city copy it
	// answers, so the assertions above are leg ORDER and nothing else.
	rigOnly := warmClaimProbeFor(
		residencyTestConfig(),
		beads.NewMemStore(),
		map[string]beads.Store{"alpha": beads.NewMemStoreFrom(0, []beads.Bead{{ID: "ra-1", Status: "open"}}, nil)},
		nil,
	)
	if !rigOnly(warmBindStampedSession("ra-1", "rig:alpha")) {
		t.Error("rig shadow leg lost: a rig-resident open trigger must still read unclaimed")
	}
}

// A rig-resident trigger whose id falls OUTSIDE that rig's effective prefix is out
// of the by-id plan by construction — shadowLegsCovering is IDInNamespace-gated —
// so it fails closed to no nudge. Pinned because it is the one population the
// deleted rig:<name> stamp parser reached and the resolver does not.
func TestBuildWarmClaimTriggerProbe_RigTriggerOutsideRigPrefixIsOutOfPlan(t *testing.T) {
	outside := beads.Bead{ID: "zz-1", Status: "open"}
	rigs := map[string]beads.Store{"alpha": beads.NewMemStoreFrom(0, []beads.Bead{outside}, nil)}

	// "by construction" is the claim, so assert the construction: rig alpha holds
	// zz-1 and is still absent from the plan, which is a work-only leg list.
	cr := warmClaimRuntimeFor(residencyTestConfig(), beads.NewMemStore(), rigs, nil)
	const wantPlan = `FirstOwner: ""[WorkFallback,Fatal]`
	if got := warmClaimByIDPlan(t, cr, "zz-1"); got != wantPlan {
		t.Fatalf("by-id plan = %q, want %q", got, wantPlan)
	}

	probe := warmClaimProbeFor(residencyTestConfig(), beads.NewMemStore(), rigs, nil)
	if probe(warmBindStampedSession("zz-1", "rig:alpha")) {
		t.Error("a rig bead outside its rig's prefix has no shadow leg; the probe must fail closed rather than answer from it")
	}

	// Control: the same id in the work ledger resolves, so the miss above is the
	// prefix gate and not a probe that stopped reading anything.
	inWork := warmClaimProbeFor(
		residencyTestConfig(),
		beads.NewMemStoreFrom(0, []beads.Bead{outside}, nil),
		rigs,
		nil,
	)
	if !inWork(warmBindStampedSession("zz-1", "rig:alpha")) {
		t.Error("an open work-ledger trigger must read unclaimed")
	}
}

// A resolution the topology cannot answer reaches the probe as an ERROR, not as a
// miss, and the probe declines: never nudge on uncertainty. Asserting both halves
// is the point — a future change to Plan's refusal gate or to PolicyFatal handling
// that turned either case into beads.ErrNotFound would leave the probe's false
// intact while quietly demoting a fault to an absence.
func TestBuildWarmClaimTriggerProbe_ResolutionErrorsNeverNudge(t *testing.T) {
	cases := map[string]struct {
		trigger  string
		resolver warmClaimTriggerResolver
	}{
		// A refused city routes its relocated classes at a store that answers
		// every read with the standing refusal. gcg- is inside the binding's
		// reserved namespace, so that binding LEADS the plan under PolicyFatal.
		"refused binding": {
			trigger: "gcg-1",
			resolver: warmClaimResolverFor(
				residencyTestConfig(),
				beads.NewMemStore(),
				nil,
				splitRoutes(refusedClassStore{err: standingStorageRefusal{err: errStorageRefusedForTest{}}}),
			),
		},
		// A suspended rig is routinely DARK: its store returns the open error on
		// every read. ra-1 is inside alpha's prefix, so that dark leg is planned.
		"dark rig leg": {
			trigger: "ra-1",
			resolver: warmClaimResolverFor(
				residencyTestConfig(),
				beads.NewMemStore(),
				map[string]beads.Store{"alpha": unavailableStore{err: errors.New("open rig store alpha: no such file or directory")}},
				nil,
			),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tc.resolver(tc.trigger)
			if err == nil {
				t.Fatalf("resolving %s: want an error, got a resolved bead", tc.trigger)
			}
			if errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("resolving %s: an unreadable leg reported absence (%v); a fault must never read as a miss", tc.trigger, err)
			}
			if buildWarmClaimTriggerProbe(tc.resolver, io.Discard)(warmBindStampedSession(tc.trigger, "")) {
				t.Errorf("%s: the probe nudged on an unresolvable trigger", name)
			}
		})
	}
}

// A resolution fault is reported ONCE per probe — the probe is built once per
// beadReconcileTick, so that is once per tick — rather than once per pool slot: a
// refused city or a dark binding suppresses the nudge for every slot at once, and
// a per-session line would be a fleet-sized log burst of one fact. An expected
// miss stays silent, because a trigger no leg holds is ordinary.
func TestBuildWarmClaimTriggerProbe_ReportsResolutionFaultOncePerTick(t *testing.T) {
	var log bytes.Buffer
	dark := warmClaimResolverFor(
		residencyTestConfig(),
		beads.NewMemStore(),
		map[string]beads.Store{"alpha": unavailableStore{err: errors.New("open rig store alpha: no such file or directory")}},
		nil,
	)
	probe := buildWarmClaimTriggerProbe(dark, &log)
	for range 3 {
		if probe(warmBindStampedSession("ra-1", "rig:alpha")) {
			t.Fatal("dark rig leg: the probe must decline")
		}
	}
	if got := strings.Count(log.String(), "\n"); got != 1 {
		t.Fatalf("reported %d lines for one standing fault across 3 slots, want 1:\n%s", got, log.String())
	}
	if !strings.Contains(log.String(), "ra-1") {
		t.Errorf("the report names no trigger, so it cannot be acted on: %q", log.String())
	}

	// An ordinary miss is not a fault and stays silent.
	var quiet bytes.Buffer
	miss := buildWarmClaimTriggerProbe(warmClaimResolverFor(residencyTestConfig(), beads.NewMemStore(), nil, nil), &quiet)
	if miss(warmBindStampedSession("nobody-holds-me", "")) {
		t.Fatal("missing trigger: the probe must decline")
	}
	if quiet.Len() != 0 {
		t.Errorf("a trigger no leg holds logged %q; only faults are worth a line", quiet.String())
	}
}
