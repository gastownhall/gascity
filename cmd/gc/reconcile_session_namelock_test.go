package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// holdName holds city's runtime lease flock on name, as another effect or
// a reaper does, until release or the test's end.
func holdName(t *testing.T, city, name string) (release func()) {
	t.Helper()
	l, err := session.TryRuntimeLease(nil, session.RuntimeLeaseRequest{City: city, Name: name})
	if err != nil {
		t.Fatalf("holding %q: %v", name, err)
	}
	t.Cleanup(l.Release)
	return l.Release
}

// nameHeld reports whether city's runtime name is held: its flock taken.
func nameHeld(t *testing.T, city, name string) bool {
	t.Helper()
	l, err := session.TryRuntimeLease(nil, session.RuntimeLeaseRequest{City: city, Name: name})
	if errors.Is(err, session.ErrRuntimeLeaseBusy) {
		return true
	}
	if err != nil {
		t.Fatalf("probing %q: %v", name, err)
	}
	l.Release()
	return false
}

// Kills an effect name lock keyed apart from the legacy reaper's (P4 F14):
// a name an effect holds stops no reap in its city and only there, and a
// name the reaper holds refuses the effect.
func TestLockRuntimeNameKeysAsTheReaper(t *testing.T) {
	cityA, cityB := t.TempDir(), t.TempDir()
	w := &World{CityPath: cityA}
	name, lease, cause, _ := lockRuntimeName(w, session.Info{SessionName: " named-1 "}, nil, 0)
	if cause != "" || lease == nil || name != "named-1" {
		t.Fatalf("lockRuntimeName = %q, %v, %q; want named-1 locked", name, lease != nil, cause)
	}
	sp := boundFake(t, "old")
	if stopped, _ := stopStillBoundClosedRuntimeLeased(nil, cityA, "named-1", "", "old", sp, false, nil); stopped || sp.CountCalls("Stop", "named-1") != 0 {
		t.Fatal("the reaper stopped a name an effect holds")
	}
	if stopped, _ := stopStillBoundClosedRuntimeLeased(nil, cityB, "named-1", "", "old", sp, false, nil); !stopped {
		t.Fatal("an effect's lock in one city blocked another city's reap of the same name")
	}
	lease.Release()

	holdName(t, cityA, "named-1") // the reaper's own lock
	if _, lease, cause, _ := lockRuntimeName(w, session.Info{SessionName: "named-1"}, nil, 0); cause != causeNameBusy || lease != nil {
		t.Fatalf("the effect took a name the reaper holds: cause %q", cause)
	}
}

// Kills a lock helper that locks the empty name (every nameless row would
// share one lock) or hands out a busy name.
func TestLockRuntimeNameRefusesEmptyOrBusy(t *testing.T) {
	w := &World{CityPath: t.TempDir()}
	for _, empty := range []string{"", "  "} {
		if name, lease, cause, _ := lockRuntimeName(w, session.Info{SessionName: empty}, nil, 0); cause != causeRouteUnknown || lease != nil || name != "" {
			t.Errorf("SessionName %q: lockRuntimeName = %q, %v, %q; want refused %q", empty, name, lease != nil, cause, causeRouteUnknown)
		}
	}
	_, lease, cause, _ := lockRuntimeName(w, session.Info{SessionName: "s-busy"}, nil, 0)
	if cause != "" {
		t.Fatalf("a free name refused: %q", cause)
	}
	if name, again, cause, _ := lockRuntimeName(w, session.Info{SessionName: "s-busy"}, nil, 0); cause != causeNameBusy || again != nil || name != "s-busy" {
		t.Errorf("busy: lockRuntimeName = %q, %v, %q; want s-busy refused", name, again != nil, cause)
	}
	lease.Release()
	if _, again, cause, _ := lockRuntimeName(w, session.Info{SessionName: "s-busy"}, nil, 0); cause != "" {
		t.Errorf("a released name refused: %q", cause)
	} else {
		again.Release()
	}
}
