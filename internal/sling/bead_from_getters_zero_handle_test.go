package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBeadFromGettersSkipsAbsentStores pins R11 (typed-stores T6a): a zero
// class handle, or a typed-nil store, boxed into a BeadQuerier is non-nil and
// panics on its first Get. BeadFromGetters skips it as it skips nil and finds
// the bead in the next getter.
func TestBeadFromGettersSkipsAbsentStores(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "b"})
	if err != nil {
		t.Fatal(err)
	}
	var typedNil *beads.MemStore
	for name, absent := range map[string]BeadQuerier{
		"nil":                       nil,
		"a zero work handle":        beads.WorkStore{},
		"a zero graph handle":       beads.GraphStore{},
		"a handle over a typed nil": beads.WorkStore{Store: typedNil},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: BeadFromGetters panicked: %v", name, r)
				}
			}()
			got, ok := BeadFromGetters(b.ID, absent, store)
			if !ok || got.ID != b.ID {
				t.Errorf("%s: BeadFromGetters = %v, %v; want %s from the next getter", name, got.ID, ok, b.ID)
			}
		}()
	}
}
