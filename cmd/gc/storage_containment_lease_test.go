package main

// Tests for the controller containment lease (#9237 follow-up, slice 1):
// boot-check-design.md §4.7 T5 (controller) and T6 (quiescence).

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// sideEffectOnFirstListStore runs before once, before its first List call,
// then delegates every call (including that one) to Store.
type sideEffectOnFirstListStore struct {
	beads.Store
	before func()
	ran    bool
}

func (s *sideEffectOnFirstListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !s.ran {
		s.ran = true
		s.before()
	}
	return s.Store.List(q)
}

// rewriteInfraManifestInPlace rewrites target's manifest with the ids it
// already records, unchanged: a no-op in content, but a real temp+rename
// like any other rewrite (infra_copy_manifest.go), so it changes the
// manifest's inode and therefore its containment-basis stamp.
func rewriteInfraManifestInPlace(t *testing.T, target infraBindingTarget) {
	t.Helper()
	manifest, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil || !recorded {
		t.Fatalf("reading the manifest to rewrite it: recorded=%v err=%v", recorded, err)
	}
	ids := make([]string, 0, len(manifest.IDs))
	for id := range manifest.IDs {
		ids = append(ids, id)
	}
	if err := rewriteInfraCopyManifest(target, manifest, ids, manifest.Created); err != nil {
		t.Fatalf("rewriting the manifest: %v", err)
	}
}

// TestConfirmInfraContainmentCapturesTheBasisBeforeTheRead is T5's "basis
// taken before the read": if the manifest is rewritten during the
// work-store List call classifyInfraContainmentGap makes, the report
// carries the manifest's stamp from BEFORE that call, not after. Mutant 13
// ("basis taken after the read") moves the capture past the read and this
// test catches it directly, by comparing the two possible answers.
func TestConfirmInfraContainmentCapturesTheBasisBeforeTheRead(t *testing.T) {
	cityPath, _, source, target := convergedInfraCity(t)

	before, err := containmentBasisStamp(target.ManifestPath(), true)
	if err != nil {
		t.Fatalf("stamping the manifest before the check: %v", err)
	}

	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return &sideEffectOnFirstListStore{Store: source, before: func() {
			rewriteInfraManifestInPlace(t, target)
		}}, nil
	}

	report := confirmInfraContainment(cityPath, target, "gc start", io.Discard)

	after, err := containmentBasisStamp(target.ManifestPath(), true)
	if err != nil {
		t.Fatalf("stamping the manifest after the check: %v", err)
	}
	if after == before {
		t.Fatal("fixture: rewriting the manifest in place did not change its stamp")
	}
	if report.basis.Manifest != before {
		t.Errorf("report.basis.Manifest = %q, want the pre-read stamp %q (the post-read stamp is %q)", report.basis.Manifest, before, after)
	}
}

// TestStrandRecheckPublishesTheLeaseAndRefreshesItsAge is T5's remaining
// controller bullets: the boot verdict is published; a definitive recheck
// tick refreshes the lease's age; a transient failure does not. Mutant 12
// ("publish on a transient tick") is caught by the final assertion.
func TestStrandRecheckPublishesTheLeaseAndRefreshesItsAge(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	publishBootContainmentLease(routes)
	boot := routes.lease.Load()
	if boot == nil {
		t.Fatal("the boot verdict published no lease")
	}
	if boot.Outcome != infraMigrationConverged.String() {
		t.Errorf("the boot lease's outcome = %q, want %q", boot.Outcome, infraMigrationConverged.String())
	}

	rec := events.NewFake()
	s := newStrandRecheck(routes, cityPath, rec, io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}

	time.Sleep(2 * time.Millisecond) // make TakenAt's advance unambiguous
	s.tick()
	afterTick := routes.lease.Load()
	if afterTick == nil || !afterTick.TakenAt.After(boot.TakenAt) {
		t.Fatalf("a definitive tick did not refresh the lease: boot taken at %v, after tick %v", boot.TakenAt, afterTick.TakenAt)
	}

	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return nil, errors.New(`Get "https://hosted-bd/v1/issues": dial tcp: i/o timeout`)
	}
	s.tick()
	openInfraMigrationSource = prev

	stillAfterTick := routes.lease.Load()
	if !stillAfterTick.TakenAt.Equal(afterTick.TakenAt) {
		t.Fatalf("a transient recheck failure refreshed the lease: %v became %v", afterTick.TakenAt, stillAfterTick.TakenAt)
	}
}

// TestServedContainmentLeaseAnswersOnlyForItsOwnCity is T5's "with two
// cities, each handler answers only for its own" (mutant 14).
func TestServedContainmentLeaseAnswersOnlyForItsOwnCity(t *testing.T) {
	cityA, cfgA, _, _ := convergedInfraCity(t)
	cityB, cfgB, _, _ := convergedInfraCity(t)

	routesA, err := storageBootGate(cityA, cfgA, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("city A refused: %v", err)
	}
	t.Cleanup(func() { _ = routesA.close() })
	routesB, err := storageBootGate(cityB, cfgB, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("city B refused: %v", err)
	}
	t.Cleanup(func() { _ = routesB.close() })

	publishBootContainmentLease(routesA)
	publishBootContainmentLease(routesB)

	registerResidencyRoutes(cityA, routesA, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(cityA, routesA) })
	registerResidencyRoutes(cityB, routesB, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(cityB, routesB) })

	leaseA, ok := servedContainmentLease(cityA)
	if !ok {
		t.Fatal("no lease served for city A")
	}
	leaseB, ok := servedContainmentLease(cityB)
	if !ok {
		t.Fatal("no lease served for city B")
	}
	if leaseA.Database == leaseB.Database {
		t.Fatal("fixture: both cities resolved the same binding database")
	}
	if leaseA.Database != routesA.target.Database {
		t.Errorf("city A's lease named database %q, want its own %q", leaseA.Database, routesA.target.Database)
	}
	if leaseB.Database != routesB.target.Database {
		t.Errorf("city B's lease named database %q, want its own %q", leaseB.Database, routesB.target.Database)
	}
}

// TestStrandRecheckSkipsTheReadWhileQuiescent is T6 (F3, mutant 17): no
// recheck runs while the city is quiescent, proved directly by counting
// work-store opens, not just by the absence of a published change.
func TestStrandRecheckSkipsTheReadWhileQuiescent(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	rec := events.NewFake()
	s := newStrandRecheck(routes, cityPath, rec, io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck for a converged city")
	}

	opens := 0
	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(path string) (beads.Store, error) {
		opens++
		return prev(path)
	}

	quiescent := true
	s.quiescent = func() bool { return quiescent }

	s.tick()
	if opens != 0 {
		t.Fatalf("a quiescent recheck opened the work store %d time(s), want 0", opens)
	}

	quiescent = false
	s.tick()
	if opens != 1 {
		t.Fatalf("a non-quiescent recheck opened the work store %d time(s), want exactly 1", opens)
	}
}
