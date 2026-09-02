package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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

// warmClaimProbeFor builds the probe the reconciler installs, through the same
// controller topology constructor the tick uses. Building it from a real
// CityRuntime rather than from a hand-assembled storeref.Topology is what makes
// these tests assert the production wiring: a topology written out here would
// keep passing after the controller stopped building one.
func warmClaimProbeFor(cfg *config.City, work beads.Store, rigs map[string]beads.Store, routes *storageRoutes) warmClaimTriggerProbe {
	cr := &CityRuntime{
		cfg:                 cfg,
		standaloneCityStore: work,
		standaloneRigStores: rigs,
		storageRoutes:       routes,
	}
	return buildWarmClaimTriggerProbe(cr.newWarmClaimTriggerResolver(cr.rigBeadStores()))
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
