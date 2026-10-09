package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// Kills an effect name lock keyed apart from the legacy reaper's (P4 F14):
// a name an effect holds stops no reap in its city and only there, and a
// name the reaper holds refuses the effect.
func TestLockRuntimeNameKeysAsTheReaper(t *testing.T) {
	w := &World{CityPath: "city-a"}
	name, unlock, ok := lockRuntimeName(w, session.Info{SessionName: " named-1 "})
	if !ok || unlock == nil || name != "named-1" {
		t.Fatalf("lockRuntimeName = %q, %v, %v; want named-1 locked", name, unlock != nil, ok)
	}
	sp := boundFake(t, "old")
	if stopped, _ := stopStillBoundClosedRuntime("city-a", "named-1", "old", sp, false); stopped || sp.CountCalls("Stop", "named-1") != 0 {
		t.Fatal("the reaper stopped a name an effect holds")
	}
	if stopped, _ := stopStillBoundClosedRuntime("city-b", "named-1", "old", sp, false); !stopped {
		t.Fatal("an effect's lock in one city blocked another city's reap of the same name")
	}
	unlock()

	held := runtimeNames.tryLock("city-a", "named-1") // the reaper's own call
	if held == nil {
		t.Fatal("the effect's unlock left the name held")
	}
	defer held()
	if _, unlock, ok := lockRuntimeName(w, session.Info{SessionName: "named-1"}); ok || unlock != nil {
		t.Fatal("the effect took a name the reaper holds")
	}
}

// Kills a lock helper that locks the empty name (every nameless row would
// share one lock) or hands out a busy name.
func TestLockRuntimeNameRefusesEmptyOrBusy(t *testing.T) {
	w := &World{CityPath: t.Name()}
	for _, empty := range []string{"", "  "} {
		if name, unlock, ok := lockRuntimeName(w, session.Info{SessionName: empty}); ok || unlock != nil || name != "" {
			t.Errorf("SessionName %q: lockRuntimeName = %q, %v, %v; want refused", empty, name, unlock != nil, ok)
		}
	}
	if free := runtimeNames.tryLock(t.Name(), ""); free == nil {
		t.Error("a refused empty name took the empty name's lock")
	} else {
		free()
	}

	_, unlock, ok := lockRuntimeName(w, session.Info{SessionName: "s-busy"})
	if !ok {
		t.Fatal("a free name refused")
	}
	if name, again, ok := lockRuntimeName(w, session.Info{SessionName: "s-busy"}); ok || again != nil || name != "s-busy" {
		t.Errorf("busy: lockRuntimeName = %q, %v, %v; want s-busy refused", name, again != nil, ok)
	}
	unlock()
	if _, again, ok := lockRuntimeName(w, session.Info{SessionName: "s-busy"}); !ok {
		t.Error("a released name refused")
	} else {
		again()
	}
}
