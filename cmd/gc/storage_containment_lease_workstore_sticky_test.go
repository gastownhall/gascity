package main

// MUST 1 (slice 2 review, owner decision 2026-11-19): the work-store
// exclusion (design doc carry-over 3) must hold end to end, over the wire,
// for every way a recheck tick can fail reading the work store — a
// transient error opening it, a transient error reading it, and a panic
// inside the read, whose origin tick() cannot know and so must attribute to
// the work store rather than assume innocent of it — and it must stay true
// (RecheckFailureFromWorkStore sticky) across a failing streak that later
// fails for an unrelated reason.

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// workStoreStickyFixture boots a converged city for real, runs one real,
// definitive recheck tick (publishing a serving Converged lease), and
// returns everything a caller needs to keep ticking it and to check what a
// one-shot process does with the lease that results.
func workStoreStickyFixture(t *testing.T) (cityPath string, target infraBindingTarget, s *strandRecheck) {
	t.Helper()
	cityPath, _, target, _ = convergedCityWithRecheckLease(t)
	s = newStrandRecheckFixture(cityPath, target)
	s.tick()
	if lease := s.routes.lease.Load(); lease == nil || !lease.Serving {
		t.Fatalf("fixture: first tick did not publish a serving lease: %+v", lease)
	}
	return cityPath, target, s
}

// newStrandRecheckFixture is the bare strandRecheck shape this file's tests
// drive by hand, ticking it directly rather than through run()'s ticker.
func newStrandRecheckFixture(cityPath string, target infraBindingTarget) *strandRecheck {
	return &strandRecheck{
		cityPath: cityPath, target: target, stderr: io.Discard, logPrefix: "gc start",
		routes: &storageRoutes{target: target},
	}
}

// acceptsOverTheWire captures this process's own basis and asks whether a
// caller fetching l over the controller socket would accept it — the same
// path a one-shot process actually takes, not a direct read of the struct.
func acceptsOverTheWire(t *testing.T, cityPath string, target infraBindingTarget, l *containmentLease) bool {
	t.Helper()
	if l == nil {
		return false
	}
	basis, err := captureContainmentBasis(cityPath, target)
	if err != nil {
		t.Fatalf("capturing this process's own basis: %v", err)
	}
	_, ok := acceptContainmentLease(l.wire(time.Now()), target, basis)
	return ok
}

type stickyPanicListStore struct{ beads.Store }

func (stickyPanicListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	panic("sticky test: bad row in the work store")
}

func setInfraMigrationSourceForTest(t *testing.T, f func(string) (beads.Store, error)) {
	t.Helper()
	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = f
}

// TestStrandRecheckWorkStoreOpenTransientRefusedOverTheWire is carry-over 3
// end to end: a transient fault OPENING the work store ("unexpected EOF",
// R5b's truncated-Dolt-file case) must produce a lease a real caller, over
// the wire, refuses — not merely a struct whose fields happen to be right.
func TestStrandRecheckWorkStoreOpenTransientRefusedOverTheWire(t *testing.T) {
	cityPath, target, s := workStoreStickyFixture(t)
	setInfraMigrationSourceForTest(t, func(string) (beads.Store, error) {
		return nil, fmt.Errorf("bd list: %w", io.ErrUnexpectedEOF)
	})
	s.tick()
	l := s.routes.lease.Load()
	if l == nil || !l.RecheckFailing {
		t.Fatalf("lease = %+v, want an extended recheck_failing lease", l)
	}
	if !l.RecheckFailureFromWorkStore || !l.wire(time.Now()).RecheckFailingFromWorkStore {
		t.Errorf("a transient fault opening the work store was not flagged from-work-store, on the lease or the wire")
	}
	if acceptsOverTheWire(t, cityPath, target, l) {
		t.Errorf("a caller, over the wire, accepted a recheck_failing lease whose failure was opening the work store")
	}
}

// TestStrandRecheckWorkStorePanicRefusedOverTheWire proves a panic inside
// the work store's own List is attributed to the work store (tick()'s
// recover path passes fromWorkStore=true: a panic's origin is unknown, so
// it is never assumed innocent) and so refused over the wire, exactly as a
// returned error would be.
func TestStrandRecheckWorkStorePanicRefusedOverTheWire(t *testing.T) {
	cityPath, target, s := workStoreStickyFixture(t)
	setInfraMigrationSourceForTest(t, func(string) (beads.Store, error) {
		return stickyPanicListStore{Store: beads.NewMemStore()}, nil
	})
	s.tick()
	l := s.routes.lease.Load()
	if l == nil || !l.RecheckFailing {
		t.Fatalf("lease = %+v, want an extended recheck_failing lease", l)
	}
	if !l.RecheckFailureFromWorkStore {
		t.Errorf("a panic inside the work-store read was not flagged from-work-store: %+v", l)
	}
	if acceptsOverTheWire(t, cityPath, target, l) {
		t.Errorf("a caller, over the wire, accepted a lease kept alive by a panic inside the work-store read")
	}
}

// TestStrandRecheckWorkStoreFlagIsStickyAcrossAFailingStreak is the sticky
// half of MUST 1: once a failing streak's failure is attributed to the work
// store, a LATER attempt in the same streak that fails for an unrelated
// reason must not clear the flag — the same stale lease is still not safe
// to trust, because the work-store fault earlier in the streak is still the
// true, unresolved cause of its staleness.
func TestStrandRecheckWorkStoreFlagIsStickyAcrossAFailingStreak(t *testing.T) {
	cityPath, target, s := workStoreStickyFixture(t)
	setInfraMigrationSourceForTest(t, func(string) (beads.Store, error) {
		return nil, fmt.Errorf("bd list: %w", io.ErrUnexpectedEOF)
	})
	s.tick()
	first := s.routes.lease.Load()
	if first == nil || !first.RecheckFailureFromWorkStore {
		t.Fatalf("fixture: first failing tick not flagged from-work-store: %+v", first)
	}
	// A second failing attempt, NOT attributed to the work store.
	s.extendLeaseOnTransientFailure(time.Now(), infraMigrationReport{Outcome: infraMigrationUncheckable, Transient: true}.FaultFromWorkStore)
	second := s.routes.lease.Load()
	if !second.RecheckFailureFromWorkStore {
		t.Fatalf("the work-store flag cleared after a later, unrelated failure: %+v", second)
	}
	if acceptsOverTheWire(t, cityPath, target, second) {
		t.Errorf("after a work-store fault, a later non-work-store failure made the same stale lease acceptable again")
	}
}

// TestStrandRecheckWorkStoreFlagClearsOnlyOnADefinitiveTick is the other
// side: only a fresh, definitive tick (publishLease, called from a
// successful tick()) clears RecheckFailureFromWorkStore — never another
// transient failure, sticky or otherwise.
func TestStrandRecheckWorkStoreFlagClearsOnlyOnADefinitiveTick(t *testing.T) {
	_, _, s := workStoreStickyFixture(t)
	setInfraMigrationSourceForTest(t, func(string) (beads.Store, error) {
		return nil, fmt.Errorf("bd list: %w", io.ErrUnexpectedEOF)
	})
	s.tick()
	if l := s.routes.lease.Load(); l == nil || !l.RecheckFailureFromWorkStore {
		t.Fatalf("fixture: first failing tick not flagged from-work-store: %+v", l)
	}
	// Restore a healthy work store and tick again: a DEFINITIVE verdict
	// republishes the lease from scratch.
	setInfraMigrationSourceForTest(t, func(string) (beads.Store, error) { return beads.NewMemStore(), nil })
	s.tick()
	l := s.routes.lease.Load()
	if l == nil || l.RecheckFailing || l.RecheckFailureFromWorkStore {
		t.Fatalf("a definitive tick did not clear the sticky work-store flag: %+v", l)
	}
}
