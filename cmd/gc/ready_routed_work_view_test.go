package main

import (
	"io"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

// readyRoutedWorkViewRuntime builds the runtime the view reads through. Each rig
// store is keyed by its BARE rig name and registered as a configured rig, which
// is how controllerState.buildStores and buildStandaloneRigStores index them in
// production — the view must not inherit that private map vocabulary.
func readyRoutedWorkViewRuntime(t *testing.T, city beads.Store, rigs map[string]beads.Store) *CityRuntime {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: readyRoutedWorkMax(3)},
		},
	}
	names := make([]string, 0, len(rigs))
	for name := range rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: filepath.Join("rigs", name)})
	}
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: cityPath,
		cfg:      cfg,
		cs: &controllerState{
			cityName:      "test-city",
			cityPath:      cityPath,
			cfg:           cfg,
			cityBeadStore: city,
			beadStores:    rigs,
			eventProv:     events.NewFake(),
		},
		stderr: io.Discard,
	}
	return cr
}

// TestReadyRoutedWorkViewCarriesExactKeysPerStore is Q2's promotion proof: the
// per-store ReadyLive read is no longer a hash input, it is the sweep's DECLARED
// routed-work view, and every unallocated row in it carries the exact
// (workID, poolTarget, sourceStore) key the pool-allocation admission is
// enqueued under.
func TestReadyRoutedWorkViewCarriesExactKeysPerStore(t *testing.T) {
	city := &readyStaticStore{Store: beads.NewMemStore(), ready: []beads.Bead{
		{ID: "w-routed", Status: "open", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}},
		{ID: "w-assigned", Status: "open", Type: "task", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "worker"}},
		{ID: "w-unrouted", Status: "open", Type: "task"},
	}}
	rig := &readyStaticStore{Store: beads.NewMemStore(), ready: []beads.Bead{
		{ID: "w-rig", Status: "open", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}},
	}}
	cr := readyRoutedWorkViewRuntime(t, city, map[string]beads.Store{"work": rig})

	view := cr.readReadyRoutedWorkView()

	if view.Stores != 2 {
		t.Fatalf("view stores = %d, want 2 (city + one rig)", view.Stores)
	}
	if len(view.Entries) != 4 {
		t.Fatalf("view entries = %+v, want every ready row in every store", view.Entries)
	}
	if city.readyCalls != 1 || rig.readyCalls != 1 {
		t.Fatalf("ReadyLive calls = (city=%d, rig=%d), want exactly one bounded read per store", city.readyCalls, rig.readyCalls)
	}

	unallocated := view.unallocated()
	if len(unallocated) != 2 {
		t.Fatalf("unallocated entries = %+v, want the two routed rows with no assignee", unallocated)
	}
	want := map[string]readyRoutedWorkEntry{
		"w-routed": {SourceStore: "city:test-city", WorkID: "w-routed", PoolTarget: "worker", Status: "open", Type: "task"},
		"w-rig":    {SourceStore: "rig:work", WorkID: "w-rig", PoolTarget: "worker", Status: "open", Type: "task"},
	}
	for _, entry := range unallocated {
		expected, ok := want[entry.WorkID]
		if !ok {
			t.Fatalf("unexpected unallocated entry %+v", entry)
		}
		if entry != expected {
			t.Fatalf("entry %q = %+v, want %+v", entry.WorkID, entry, expected)
		}
	}
}

// TestReadyRoutedWorkViewEmitsResolvableStoreRefs is the ga-f7v2ft.155 fence.
// The view's store ref is not a label, it is the KEY: the sweep forwards it into
// the pool-allocation hint verbatim and every consumer downstream resolves the
// canonical spelling and only that. So a ref this producer emits must satisfy
// all three consumers, for the city store and for every rig store alike.
//
// Each arm below is a distinct live symptom of one wrong string, which is why
// they are asserted together rather than trusted to follow from the spelling:
// the allocation cannot find the store, the drain-ack/start effect boundaries
// compare a canonicalized ROW against this raw ref, and the allocation policy
// asks whether the agent reaches the store at all.
func TestReadyRoutedWorkViewEmitsResolvableStoreRefs(t *testing.T) {
	routed := []beads.Bead{{ID: "w-1", Status: "open", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}}}
	city := &readyStaticStore{Store: beads.NewMemStore(), ready: routed}
	rig := &readyStaticStore{Store: beads.NewMemStore(), ready: routed}
	cr := readyRoutedWorkViewRuntime(t, city, map[string]beads.Store{"work": rig})
	cfg := cr.cfg

	view := cr.readReadyRoutedWorkView()
	if len(view.Entries) != 2 {
		t.Fatalf("view entries = %+v, want one row per store", view.Entries)
	}

	// The legacy demand collector's own bare vocabulary ("city", a bare rig
	// name) is what the member row is stamped from, canonicalized at the stamp
	// (canonicalTriggerWorkStoreRef). It is the row side of every scope
	// comparison the view's ref is measured against.
	legacyStoreKey := map[string]string{"city:test-city": "city", "rig:work": "work"}
	for _, entry := range view.Entries {
		if _, scoped := storeref.ScopeRigContext(entry.SourceStore); !scoped {
			t.Fatalf("view emitted %q, which carries no store scope; the shared vocabulary refuses bare refs", entry.SourceStore)
		}
		key, known := legacyStoreKey[entry.SourceStore]
		if !known {
			t.Fatalf("view emitted unexpected store ref %q", entry.SourceStore)
		}

		// (a) The allocation must find the store the key names. This is the
		// "source store %q is unavailable" refusal.
		store, ok := cr.cs.routedWorkStore(cfg, entry.SourceStore)
		if !ok || store == nil {
			t.Fatalf("routedWorkStore(%q) did not resolve; the enqueued allocation can never act", entry.SourceStore)
		}

		// (b) The effect boundaries bind the row to the lease by store scope:
		// authorizeRoutedWorkPoolStart compares the canonicalized row against
		// the RAW hint ref, and the drain-ack arm compares it against the
		// canonicalized lease ref. Both must hold.
		rowRef := canonicalizeLegacyWorkflowStoreRef(cfg, cr.cityPath, key)
		if rowRef != entry.SourceStore {
			t.Fatalf("row ref %q != view ref %q: the pool-allocation start binding refuses this member forever", rowRef, entry.SourceStore)
		}
		if leaseRef := canonicalizeLegacyWorkflowStoreRef(cfg, cr.cityPath, entry.SourceStore); rowRef != leaseRef {
			t.Fatalf("row ref %q != drain-ack lease ref %q: the acknowledgement is refused lease_invalid", rowRef, leaseRef)
		}
	}

	// (c) The allocation policy asks whether the agent reaches the source
	// store, and a rig-less agent reaches city-scoped refs only. A bare ref
	// answers no, so the policy is unsupported and the allocation — and the
	// drain-ack authorization built on the same policy — is refused.
	cityEntry, found := "", false
	for _, entry := range view.Entries {
		if rigContext, _ := storeref.ScopeRigContext(entry.SourceStore); rigContext == "" {
			cityEntry, found = entry.SourceStore, true
		}
	}
	if !found {
		t.Fatal("the view emitted no city-store row; the fixture no longer models the allocation policy check")
	}
	if !agentutil.AgentReachesWorkflowStore(cityEntry, &cfg.Agents[0], cr.cityPath, cfg) {
		t.Fatalf("agent does not reach the view's city store ref %q; the allocation policy refuses it", cityEntry)
	}
}

// TestReadyRoutedWorkViewFingerprintTracksDemandNotTouches pins what the
// promoted view invalidates the demand snapshot on. The retired fingerprint
// hashed each bead's UpdatedAt, so any touch rebuilt desired state for a change
// no demand decision could see. The view hashes the DECLARED projection: a
// re-route moves it, an assignment moves it, a bare touch does not.
func TestReadyRoutedWorkViewFingerprintTracksDemandNotTouches(t *testing.T) {
	base := beads.Bead{
		ID: "w-1", Status: "open", Type: "task",
		UpdatedAt: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
		Metadata:  map[string]string{"gc.routed_to": "worker"},
	}
	store := &readyStaticStore{Store: beads.NewMemStore(), ready: []beads.Bead{base}}
	cr := readyRoutedWorkViewRuntime(t, store, nil)

	first := cr.readReadyRoutedWorkView().Fingerprint

	touched := base
	touched.UpdatedAt = base.UpdatedAt.Add(time.Hour)
	store.ready = []beads.Bead{touched}
	if got := cr.readReadyRoutedWorkView().Fingerprint; got != first {
		t.Fatalf("fingerprint moved on a demand-irrelevant touch: %q != %q", got, first)
	}

	assigned := base
	assigned.Assignee = "worker-1"
	store.ready = []beads.Bead{assigned}
	if got := cr.readReadyRoutedWorkView().Fingerprint; got == first {
		t.Fatal("fingerprint did not move when the routed work was allocated")
	}

	rerouted := base
	rerouted.Metadata = map[string]string{"gc.routed_to": "other"}
	store.ready = []beads.Bead{rerouted}
	if got := cr.readReadyRoutedWorkView().Fingerprint; got == first {
		t.Fatal("fingerprint did not move when the routed work changed target")
	}
}

// TestReadyRoutedWorkViewChangeEdgeIsConsumedOnce pins the invalidation contract
// the retired snapshot field carried: the change is edge-detected at the read,
// and exactly one demand-snapshot refresh consumes it.
func TestReadyRoutedWorkViewChangeEdgeIsConsumedOnce(t *testing.T) {
	store := &readyStaticStore{Store: beads.NewMemStore(), ready: []beads.Bead{
		{ID: "w-1", Status: "open", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}},
	}}
	cr := readyRoutedWorkViewRuntime(t, store, nil)

	cr.flooredReadyRoutedWorkView()
	if cr.takeReadyRoutedWorkViewChanged() {
		t.Fatal("first observation raised a change edge; there is nothing to have changed from")
	}

	store.ready = append(store.ready, beads.Bead{
		ID: "w-2", Status: "open", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	cr.readyRoutedWorkViewAt = time.Now().Add(-2 * readyRoutedWorkViewFloor)
	cr.flooredReadyRoutedWorkView()
	if !cr.takeReadyRoutedWorkViewChanged() {
		t.Fatal("new unallocated routed work did not raise a change edge")
	}
	if cr.takeReadyRoutedWorkViewChanged() {
		t.Fatal("change edge survived the refresh that consumed it")
	}
}
