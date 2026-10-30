package api

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The worker's resolver contract is beads.ErrNotFound for an absent bead;
// resolveBeadOwner answers a typed 404 instead, so the adapter must translate
// it, and only it.
func TestFormulaBeadResolverMapsTypedNotFoundToErrNotFound(t *testing.T) {
	fs := newFakeState(t)
	city := beads.NewMemStore()
	rigStore := beads.NewMemStoreFrom(5000, []beads.Bead{{ID: "rg-1", Title: "in the rig"}}, nil)
	fs.cityBeadStore = city
	fs.stores = map[string]beads.Store{"myrig": rigStore}
	s := New(fs)
	resolve := s.formulaBeadResolver()

	if _, err := resolve("zz-404"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("resolving an absent id = %v, want beads.ErrNotFound", err)
	}
	got, err := resolve("rg-1")
	if err != nil || got.ID != "rg-1" {
		t.Fatalf("resolving a rig-resident id = %+v, %v", got, err)
	}
}

// A store that cannot be read is not "absent": the adapter must not launder it
// into beads.ErrNotFound, or the worker would cache an outage as a miss.
func TestFormulaBeadResolverKeepsStoreFailuresDistinctFromAbsence(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	fs.stores = map[string]beads.Store{"myrig": failingGetStore{Store: beads.NewMemStore()}}
	s := New(fs)

	_, err := s.formulaBeadResolver()("rg-1")
	if err == nil {
		t.Fatalf("resolving through an unreadable store succeeded")
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("a store failure was reported as beads.ErrNotFound: %v", err)
	}
}

type failingGetStore struct{ beads.Store }

func (failingGetStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("store offline")
}
