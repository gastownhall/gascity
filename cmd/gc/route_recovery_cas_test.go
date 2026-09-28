package main

import (
	"io"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// raceAfterLiveReadStore lands a competing write right after route recovery's
// live re-read of one bead and before its restore — the window a blind
// metadata write cannot see. It embeds the concrete *beads.MemStore, so the
// metadata compare-and-set capability is promoted exactly as it is on the
// production stores, and its SetMetadataBatch is an atomic merge: the
// behavior the native store has once its merge is a compare-and-swap. What is
// left to prove is route recovery's own write.
type raceAfterLiveReadStore struct {
	*beads.MemStore
	target string
	race   func(store *beads.MemStore)
	// onScan fires the race after the open-corpus scan instead of the live
	// re-read: a fenced step never reaches the re-read, so the activation
	// lands after the only read route recovery makes of it.
	onScan bool
	fired  bool
	// batches records every SetMetadataBatch key set, so a test can tell a
	// restore that wrote nothing from one that wrote an unchanged value.
	batches []map[string]string
}

func (s *raceAfterLiveReadStore) fire(ids ...string) {
	if s.fired || !slices.Contains(ids, s.target) {
		return
	}
	s.fired = true
	s.race(s.MemStore)
}

func (s *raceAfterLiveReadStore) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	s.fire(id)
	return b, err
}

func (s *raceAfterLiveReadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(q)
	switch {
	case len(q.IDs) > 0:
		// Only the re-verify names ids; the open-corpus scan does not.
		s.fire(q.IDs...)
	case s.onScan:
		for _, row := range rows {
			s.fire(row.ID)
		}
	}
	return rows, err
}

func (s *raceAfterLiveReadStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.batches = append(s.batches, kvs)
	return s.MemStore.SetMetadataBatch(id, kvs)
}

const routeRecoveryQualifiedRoute = "rig/" + routeRecoveryTestPool

// activateFencedStep is molecule's graph activation: the withheld route comes
// back qualified and the fence keys are cleared, in one update.
func activateFencedStep(id string) func(*beads.MemStore) {
	return func(store *beads.MemStore) {
		task := "task"
		if err := store.Update(id, beads.UpdateOpts{Type: &task, Metadata: map[string]string{
			beadmeta.InstantiatingMetadataKey:    "",
			beadmeta.DeferredRoutedToMetadataKey: "",
			beadmeta.DeferredTypeMetadataKey:     "",
			beadmeta.RoutedToMetadataKey:         routeRecoveryQualifiedRoute,
		}}); err != nil {
			panic(err)
		}
	}
}

// routeConcurrently is any other writer that routes the bead after the live
// read: a router, a sling, another pass.
func routeConcurrently(id string) func(*beads.MemStore) {
	return func(store *beads.MemStore) {
		if err := store.SetMetadata(id, beadmeta.RoutedToMetadataKey, routeRecoveryQualifiedRoute); err != nil {
			panic(err)
		}
	}
}

// TestRouteRecoveryKeepsARouteStampedAfterItsLiveRead is the combined proof
// for the lost-workflow incident (#6221/#6222): route recovery racing the
// writer that routes the same bead, on a store whose metadata merge is
// already atomic.
//
// An atomic merge is not enough on its own. Route recovery computed its route
// from a live row whose gc.routed_to was empty; if another writer routes the
// bead before the restore lands, a merge still writes gc.routed_to — the bare
// pool name — over the qualified route, and the workflow is as lost as if the
// fence had come back. So the restore is a compare-and-set of gc.routed_to
// from empty: the route another writer stamped stands, and the lost race is a
// no-op, not an error and not a restore.
//
// The fenced-step case is the incident itself; there the fence predicate
// (#6231) already keeps route recovery away, and the compare-and-set is the
// second line if the live read predates the activation.
func TestRouteRecoveryKeepsARouteStampedAfterItsLiveRead(t *testing.T) {
	cases := []struct {
		name string
		// seed builds a fresh bead per subtest: the store keeps the seed's
		// metadata map, so a shared seed would carry one subtest's race into
		// the next.
		seed   func() beads.Bead
		race   func(string) func(*beads.MemStore)
		onScan bool
	}{
		{
			name: "fenced graph step activated",
			seed: func() beads.Bead {
				return fencedGraphStepBead("T-race", map[string]string{
					beadmeta.InstantiatingMetadataKey:    "true",
					beadmeta.DeferredRoutedToMetadataKey: routeRecoveryQualifiedRoute,
					beadmeta.DeferredTypeMetadataKey:     "task",
				})
			},
			race:   activateFencedStep,
			onScan: true,
		},
		{
			name: "unfenced bead routed by another writer",
			seed: func() beads.Bead { return unroutedWorkBead("T-race") },
			race: routeConcurrently,
		},
	}
	passes := map[string]func(cr *CityRuntime, seed beads.Bead) routeRecoveryReport{
		"backstop": func(cr *CityRuntime, _ beads.Bead) routeRecoveryReport {
			return cr.runRouteRecoveryBackstop(backstopReasonCadence)
		},
		"delta": func(cr *CityRuntime, seed beads.Bead) routeRecoveryReport {
			// The delta lane re-verifies whatever the journal named; name the
			// bead directly so the race is exercised even when the event-feed
			// predicate would have filtered it.
			lane := cr.routeRecoveryLaneOf()
			lane.mu.Lock()
			lane.pending[seed.ID] = struct{}{}
			lane.mu.Unlock()
			return cr.recoverUnroutedWorkRoutesDelta()
		},
	}
	for _, tc := range cases {
		for passName, pass := range passes {
			t.Run(tc.name+"/"+passName, func(t *testing.T) {
				seed := tc.seed()
				store := &raceAfterLiveReadStore{
					MemStore: beads.NewMemStoreFrom(0, []beads.Bead{seed}, nil),
					target:   seed.ID,
					race:     tc.race(seed.ID),
					onScan:   tc.onScan,
				}
				cr := &CityRuntime{cityName: "city", standaloneCityStore: store, stderr: io.Discard}

				report := pass(cr, seed)
				if !store.fired {
					t.Fatal("the competing write never ran; the pass did not re-read the bead")
				}
				if report.err != nil {
					t.Fatalf("pass error = %v, want nil: losing the race to another writer is not an error", report.err)
				}
				if report.restored != 0 {
					t.Fatalf("pass restored %d route(s), want 0: the bead was routed by the time the restore landed", report.restored)
				}
				if got := mustRoutedTo(t, store.MemStore, seed.ID); got != routeRecoveryQualifiedRoute {
					t.Fatalf("gc.routed_to = %q, want %q: route recovery replaced the route another writer stamped after its live read", got, routeRecoveryQualifiedRoute)
				}
				if len(store.batches) != 0 {
					t.Fatalf("route recovery issued blind metadata write(s) %v, want none", store.batches)
				}
				b, err := store.MemStore.Get(seed.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				for _, key := range []string{beadmeta.InstantiatingMetadataKey, beadmeta.DeferredRoutedToMetadataKey} {
					if v := b.Metadata[key]; v != "" {
						t.Fatalf("%s = %q after the pass, want empty: the fence came back", key, v)
					}
				}
			})
		}
	}
}

// TestRouteRecoveryLostRaceIsNotCountedAsARestore pins that a restore whose
// compare-and-set lost to another writer does not feed the flap bound: that
// bound exists to catch a sibling lane clearing routes this lane stamped, and
// a route this lane never stamped is not that.
func TestRouteRecoveryLostRaceIsNotCountedAsARestore(t *testing.T) {
	seed := unroutedWorkBead("T-race")
	store := &raceAfterLiveReadStore{
		MemStore: beads.NewMemStoreFrom(0, []beads.Bead{seed}, nil),
		target:   seed.ID,
		race:     routeConcurrently(seed.ID),
	}
	cr := &CityRuntime{cityName: "city", standaloneCityStore: store, stderr: io.Discard}
	cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if !store.fired {
		t.Fatal("the competing write never ran; the pass did not re-read the bead")
	}

	lane := cr.routeRecoveryLaneOf()
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if n := lane.restores[seed.ID]; n != 0 {
		t.Fatalf("restore tally for a lost race = %d, want 0", n)
	}
}

// TestRouteRecoveryRestoreClearsAStaleQuarantineAfterTheSwap pins the
// compare-and-set path's second write: a quarantined bead whose re-check now
// passes gets its route AND loses the stale quarantine verdict.
func TestRouteRecoveryRestoreClearsAStaleQuarantineAfterTheSwap(t *testing.T) {
	seed := unroutedWorkBead("T-q")
	seed.Metadata[beadmeta.RouteQuarantineMetadataKey] = "true"
	seed.Metadata[beadmeta.RouteQuarantineReasonMetadataKey] = routeRecoveryQuarantineRestoreFlap
	backing := beads.NewMemStoreFrom(0, []beads.Bead{seed}, nil)
	cr := &CityRuntime{cityName: "city", standaloneCityStore: backing, stderr: io.Discard}

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if report.err != nil || report.restored != 1 {
		t.Fatalf("pass restored=%d err=%v, want 1 and nil", report.restored, report.err)
	}
	if got := mustRoutedTo(t, backing, "T-q"); got != routeRecoveryTestPool {
		t.Fatalf("gc.routed_to = %q, want %q", got, routeRecoveryTestPool)
	}
	if q := quarantineReason(t, backing, "T-q"); q != "" {
		t.Fatalf("quarantine reason = %q after a passing restore, want cleared", q)
	}
}

// casUnsupportedStore advertises the metadata compare-and-set but refuses it
// at call time, the way a store whose conditional writes are disabled at the
// instance does (and emittingClassStore over a store without the capability).
type casUnsupportedStore struct {
	beads.Store
	casCalls int
	batches  []map[string]string
}

func (s *casUnsupportedStore) CompareAndSetMetadataKey(_, _, _, _ string) (bool, error) {
	s.casCalls++
	return false, beads.ErrConditionalWriteUnsupported
}

func (s *casUnsupportedStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.batches = append(s.batches, kvs)
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestRouteRecoveryFallsBackToTheBatchWriteWhenTheCASIsUnsupported pins the
// one error that reaches the legacy write: a store that cannot compare-and-set
// after all still gets its route restored (and a stale quarantine cleared in
// the same batch), rather than route recovery stopping on it.
func TestRouteRecoveryFallsBackToTheBatchWriteWhenTheCASIsUnsupported(t *testing.T) {
	seed := unroutedWorkBead("T-fallback")
	seed.Metadata[beadmeta.RouteQuarantineMetadataKey] = "true"
	seed.Metadata[beadmeta.RouteQuarantineReasonMetadataKey] = routeRecoveryQuarantineRestoreFlap
	backing := beads.NewMemStoreFrom(0, []beads.Bead{seed}, nil)
	store := &casUnsupportedStore{Store: backing}
	if _, ok := beads.MetadataCASWriterFor(store); !ok {
		t.Fatal("fixture does not advertise the metadata CAS; the fallback would not be exercised")
	}
	cr := &CityRuntime{cityName: "city", standaloneCityStore: store, stderr: io.Discard}

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if report.err != nil || report.restored != 1 {
		t.Fatalf("pass restored=%d err=%v, want 1 and nil: an unsupported CAS must fall back, not fail", report.restored, report.err)
	}
	if store.casCalls != 1 {
		t.Fatalf("CAS calls = %d, want 1 (tried before falling back)", store.casCalls)
	}
	if len(store.batches) != 1 {
		t.Fatalf("batch writes = %d (%v), want exactly 1: the legacy single write", len(store.batches), store.batches)
	}
	want := map[string]string{
		beadmeta.RoutedToMetadataKey:              routeRecoveryTestPool,
		beadmeta.RouteQuarantineMetadataKey:       "",
		beadmeta.RouteQuarantineReasonMetadataKey: "",
	}
	for k, v := range want {
		if got, ok := store.batches[0][k]; !ok || got != v {
			t.Fatalf("fallback batch %v, want %s=%q in it", store.batches[0], k, v)
		}
	}
	if got := mustRoutedTo(t, backing, "T-fallback"); got != routeRecoveryTestPool {
		t.Fatalf("gc.routed_to = %q, want %q", got, routeRecoveryTestPool)
	}
}
