package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// stubAvailabilityGate drives a CachingStore's transport availability from a
// test. The production gate is the per-scope transport circuit breaker.
type stubAvailabilityGate struct{ available bool }

func (g stubAvailabilityGate) Available() bool { return g.available }
func (g stubAvailabilityGate) ProbeDue() bool  { return false }

func newGatedCityStore(t *testing.T, available bool) *beads.CachingStore {
	t.Helper()
	cache := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.SetAvailabilityGate(stubAvailabilityGate{available: available})
	return cache
}

// TestCityBeadsDiagnosticReportsDegradedThroughThePolicyWrapper exercises the
// surface status actually reads — controllerState.CityBeadsDiagnostic — with
// the store wrapped exactly as production wraps it. The diagnostic is captured
// at store-open time, so the degraded verdict has to be recomputed on read;
// a captured copy would report false forever.
func TestCityBeadsDiagnosticReportsDegradedThroughThePolicyWrapper(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		available bool
		want      bool
	}{
		{name: "reachable", available: true, want: false},
		{name: "unreachable", available: false, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs := &controllerState{
				cityPath:            t.TempDir(),
				cityBeadStore:       wrapStoreWithBeadPolicies(newGatedCityStore(t, tc.available), &config.City{}),
				cityBeadsDiagnostic: &beads.BeadsDiagnostic{Store: beads.BeadsStoreNameBdStore},
			}
			diag := cs.CityBeadsDiagnostic()
			if diag == nil {
				t.Fatal("CityBeadsDiagnostic() = nil, want a diagnostic")
			}
			if diag.Degraded != tc.want {
				t.Fatalf("CityBeadsDiagnostic().Degraded = %v, want %v", diag.Degraded, tc.want)
			}
			if diag.Store != beads.BeadsStoreNameBdStore {
				t.Fatalf("CityBeadsDiagnostic().Store = %q, want the captured value preserved", diag.Store)
			}
		})
	}
}

// TestCityBeadsDiagnosticDoesNotMutateTheCapturedDiagnostic pins that the
// read-time computation writes to the caller's copy only. Mutating the stored
// diagnostic would latch the first degraded reading for the process lifetime.
func TestCityBeadsDiagnosticDoesNotMutateTheCapturedDiagnostic(t *testing.T) {
	t.Parallel()
	captured := &beads.BeadsDiagnostic{Store: beads.BeadsStoreNameBdStore}
	cs := &controllerState{
		cityPath:            t.TempDir(),
		cityBeadStore:       wrapStoreWithBeadPolicies(newGatedCityStore(t, false), &config.City{}),
		cityBeadsDiagnostic: captured,
	}
	if got := cs.CityBeadsDiagnostic(); !got.Degraded {
		t.Fatal("CityBeadsDiagnostic().Degraded = false with an unreachable store")
	}
	if captured.Degraded {
		t.Fatal("the captured diagnostic was mutated; a later recovery could never clear it")
	}
}

func TestStoreReportsDegradedIsSafeOnStoresThatCannotAnswer(t *testing.T) {
	t.Parallel()
	if storeReportsDegraded(nil) {
		t.Fatal("storeReportsDegraded(nil) = true, want false")
	}
	if storeReportsDegraded(beads.NewMemStore()) {
		t.Fatal("storeReportsDegraded(store without a caching layer) = true, want false")
	}
	if storeReportsDegraded(wrapStoreWithBeadPolicies(beads.NewMemStore(), &config.City{})) {
		t.Fatal("storeReportsDegraded(policy-wrapped plain store) = true, want false")
	}
}
