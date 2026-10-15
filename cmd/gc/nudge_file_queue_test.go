package main

import (
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/storebindingtest"
)

// TestFileNudgeQueueConformance runs the Nudges class suite against the file
// queue openNudgeQueue builds, under the two legacy behaviors it declares.
// Each assertion gets a fresh city whose shadow store is one MemStore, both
// the store a caller passes and the one every self-opening verb (claim, list,
// ack, release, Maintain) opens. The second leg passes the zero store, so
// every verb opens its own, as the facades do for a caller that holds none.
// It replaces a package seam, so it is serial.
func TestFileNudgeQueueConformance(t *testing.T) {
	var mu sync.Mutex
	stores := map[string]*beads.MemStore{}
	prev := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(cityPath string) (beads.NudgesStore, beads.Store) {
		mu.Lock()
		defer mu.Unlock()
		store := stores[cityPath]
		if store == nil {
			return beads.NudgesStore{}, nil
		}
		return beads.NudgesStore{Store: store}, store
	}
	t.Cleanup(func() { openOwnedNudgeBeadStore = prev })

	for _, leg := range []struct {
		name      string
		passStore bool
	}{
		{name: "with the caller's store", passStore: true},
		{name: "with the zero store", passStore: false},
	} {
		t.Run(leg.name, func(t *testing.T) {
			storebindingtest.RunNudgeFrontDoorTests(storebindingtest.Wrap(t), storebindingtest.NudgesSuite{
				NewFrontDoors: func(tb storebindingtest.TB) storebinding.NudgeFrontDoors {
					city := tb.TempDir()
					store := beads.NewMemStore()
					mu.Lock()
					stores[city] = store
					mu.Unlock()
					nudges := beads.NudgesStore{Store: store}
					passed := beads.NudgesStore{}
					if leg.passStore {
						passed = nudges
					}
					return storebinding.NudgeFrontDoors{Queue: openNudgeQueue(city, passed).Queue, Shadows: nudgeShadows(nudges)}
				},
				Capability: storebinding.ClassCapability{Available: true},
				Policy:     storebindingtest.NudgeQueuePolicy{VerbsMaintain: true, ForgetsRetiredIDs: true},
			})
		})
	}
}
