package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// allCoordClasses is the full set of coordination classes the seam must route.
var allCoordClasses = coordclass.Classes()

// TestClassStoreForIsIdentity pins the P1 invariant: on a single-store city
// every per-class accessor returns the exact same concrete store the caller
// uses today. Non-work classes resolve to CityBeadStore(); work resolves to
// CityBeadStore() with no rig and to BeadStore(rig) with a rig.
func TestClassStoreForIsIdentity(t *testing.T) {
	st := newFakeState(t)
	city := beads.NewMemStore()
	st.cityBeadStore = city

	for _, class := range allCoordClasses {
		if got := classStoreFor(st, class, ""); !sameStore(got, city) {
			t.Errorf("classStoreFor(%s, no rig) = %p, want CityBeadStore %p", class, got, city)
		}
	}

	// Work with a rig resolves to that rig's store; every other class ignores
	// the rig and still resolves to the city store (today's behavior).
	rigStore := st.stores["myrig"]
	if got := classStoreFor(st, coordclass.ClassWork, "myrig"); !sameStore(got, rigStore) {
		t.Errorf("classStoreFor(work, myrig) = %p, want BeadStore(myrig) %p", got, rigStore)
	}
	for _, class := range allCoordClasses {
		if class == coordclass.ClassWork {
			continue
		}
		if got := classStoreFor(st, class, "myrig"); !sameStore(got, city) {
			t.Errorf("classStoreFor(%s, myrig) = %p, want CityBeadStore %p", class, got, city)
		}
	}
}

// TestClassBeadStoresForIDDedupesToSingleStore pins that prepending the class
// store collapses to one entry when the class store is already the sole by-id
// candidate — the single-store identity case.
func TestClassBeadStoresForIDDedupesToSingleStore(t *testing.T) {
	st := newFakeState(t)
	city := beads.NewMemStore()
	st.cityBeadStore = city
	// Drop the rig store so CityBeadStore is the only by-id candidate.
	st.stores = map[string]beads.Store{}
	st.cfg.Rigs = nil
	srv := &Server{state: st}

	got := srv.classBeadStoresForID(coordclass.ClassSessions, "", "sess-1")
	if len(got) != 1 {
		t.Fatalf("classBeadStoresForID returned %d stores, want 1 (dedup); got %v", len(got), got)
	}
	if !sameStore(got[0], city) {
		t.Errorf("classBeadStoresForID[0] = %p, want CityBeadStore %p", got[0], city)
	}
}

// sameStore reports pointer identity between two stores.
func sameStore(a, b beads.Store) bool {
	ka, oka := storeIdentityKey(a)
	kb, okb := storeIdentityKey(b)
	return oka && okb && ka == kb
}
