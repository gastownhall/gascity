package storebinding_test

// The beads-mode nudge front doors (the beads queue and its shadow view) run
// the shared Nudges corpus, over MemStore and over the SQLite engine.

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/storebindingtest"
)

// corpusClaimingMemStore is beads.MemStore with the two-argument assignment
// claim the beads queue requires, built on MemStore's own revision fence.
type corpusClaimingMemStore struct {
	*beads.MemStore
}

func (s corpusClaimingMemStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	for {
		current, err := s.Get(id)
		if err != nil {
			return beads.Bead{}, false, err
		}
		if current.Status != "open" && current.Status != "in_progress" {
			return beads.Bead{}, false, nil
		}
		if current.Assignee != "" && current.Assignee != assignee {
			return beads.Bead{}, false, nil
		}
		inProgress := "in_progress"
		err = s.UpdateIfMatch(id, current.Revision, beads.UpdateOpts{Assignee: &assignee, Status: &inProgress})
		var stale *beads.PreconditionFailedError
		if errors.As(err, &stale) {
			continue
		}
		if err != nil {
			return beads.Bead{}, false, err
		}
		held, err := s.Get(id)
		return held, err == nil, err
	}
}

func TestBeadsNudgeFrontDoorsRunTheNudgesCorpus(t *testing.T) {
	backends := []struct {
		name string
		open func(storebindingtest.TB) beads.Store
	}{
		{"memstore", func(storebindingtest.TB) beads.Store {
			memory := beads.NewMemStore()
			memory.HonorExplicitIDs = true
			return corpusClaimingMemStore{MemStore: memory}
		}},
		{"sqlite", openBeadsOverSQLiteEngine},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			storebindingtest.RunNudgeFrontDoorTests(storebindingtest.Wrap(t), storebindingtest.NudgesSuite{
				NewFrontDoors: func(tb storebindingtest.TB) storebinding.NudgeFrontDoors {
					fronts, err := storebinding.NewBeadsNudgeFrontDoors(beads.NudgesStore{Store: backend.open(tb)})
					if err != nil {
						tb.Fatalf("NewBeadsNudgeFrontDoors: %v", err)
					}
					return fronts
				},
				Capability: storebindingtest.ReferenceCapability,
			})
		})
	}
}
