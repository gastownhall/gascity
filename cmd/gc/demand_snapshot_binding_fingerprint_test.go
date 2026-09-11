package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
)

// T-F (D9.i): the demand snapshot may be reused for up to its max age, and what
// licenses that reuse is a fingerprint over the stores the demand probe reads.
// The probe's LEADING leg is the sessions-class store — the graph binding on a
// converged split city, where every routed graph step lives — and the
// fingerprint did not hash it. A step claimed or closed there changed nothing
// the fingerprint could see, so the controller kept asserting demand for work
// that was already taken.

// bindingFingerprintRuntime builds a CityRuntime whose sessions class is served
// from a DIFFERENT store than the work store, the way a converged split city
// serves it.
func bindingFingerprintRuntime(t *testing.T, work, binding beads.Store) *CityRuntime {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Storage:   infraSplitConfig(cityPath).Storage,
	}
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: cityPath,
		cfg:      cfg,
		cs: &controllerState{
			cityName:      "test-city",
			cityBeadStore: work,
			eventProv:     events.NewFake(),
		},
		stderr: io.Discard,
	}
	cr.storageRoutes = &storageRoutes{binding: "infra", stores: map[coordclass.Class]beads.Store{
		coordclass.ClassGraph:     binding,
		coordclass.ClassSessions:  binding,
		coordclass.ClassMessaging: binding,
		coordclass.ClassOrders:    binding,
		coordclass.ClassNudges:    binding,
	}}
	return cr
}

func routedStepIn(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	})
	if err != nil {
		t.Fatalf("seeding %q: %v", title, err)
	}
	return created
}

// TestDemandFingerprintTracksTheBindingTheProbeReads: a claim in the binding
// must invalidate the snapshot, exactly as a work-store write does.
func TestDemandFingerprintTracksTheBindingTheProbeReads(t *testing.T) {
	work := beads.NewMemStore()
	binding := beads.NewMemStore()
	cr := bindingFingerprintRuntime(t, work, binding)
	step := routedStepIn(t, binding, "routed graph step")

	before := cr.readyDemandSnapshotFingerprint()
	if stable := cr.readyDemandSnapshotFingerprint(); stable != before {
		t.Fatalf("fingerprint is not stable across reads: %q != %q", before, stable)
	}

	// The claim a worker would make: the row leaves the ready set.
	assignee := "gc__worker-1"
	inProgress := "in_progress"
	if err := binding.Update(step.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claiming the step: %v", err)
	}

	if after := cr.readyDemandSnapshotFingerprint(); after == before {
		t.Fatal("the fingerprint did not change when a routed step was claimed in the binding; demand stays asserted for work already taken")
	}
}

// Control: a write to a store the probe does NOT read must not invalidate the
// snapshot — otherwise the fix would be indistinguishable from hashing
// everything, and the 30s reuse this cache exists for would be gone.
func TestDemandFingerprintIgnoresAStoreTheProbeDoesNotRead(t *testing.T) {
	work := beads.NewMemStore()
	binding := beads.NewMemStore()
	unrelated := beads.NewMemStore()
	cr := bindingFingerprintRuntime(t, work, binding)
	routedStepIn(t, binding, "routed graph step")

	before := cr.readyDemandSnapshotFingerprint()
	routedStepIn(t, unrelated, "work in a store nothing here reads")

	if after := cr.readyDemandSnapshotFingerprint(); after != before {
		t.Fatalf("fingerprint changed for a store outside the demand probe: %q -> %q", before, after)
	}
}

// Control: on a city that relocates nothing the sessions class IS the work
// store, so it must be hashed exactly once — the dedup keeps the fix from
// doubling every fingerprint read on the common layout.
func TestDemandFingerprintReadsASingleStoreCityOnce(t *testing.T) {
	store := &countingReadyStore{Store: beads.NewMemStore()}
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: t.TempDir(),
		cfg:      &config.City{Workspace: config.Workspace{Name: "test-city"}},
		cs: &controllerState{
			cityName:      "test-city",
			cityBeadStore: store,
			eventProv:     events.NewFake(),
		},
		stderr: io.Discard,
	}

	cr.readyDemandSnapshotFingerprint()

	if store.readyCalls != 1 {
		t.Fatalf("ready reads = %d, want 1 (the sessions class is the work store here)", store.readyCalls)
	}
}

type countingReadyStore struct {
	beads.Store
	readyCalls int
}

func (s *countingReadyStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.readyCalls++
	return s.Store.Ready(q...)
}
