//go:build integration

package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// TestSeededStateClassStoresAreTheCityStore pins the typed-store T3 reroutes
// of the seeded harness: the nudge queue's shadows and the external-messaging
// services take the city's handles from its single-store Stores, and those
// are the one city store the deps hand in, exactly the store both used before.
func TestSeededStateClassStoresAreTheCityStore(t *testing.T) {
	city := beads.NewMemStore()
	s := newSeededState(SeededCityDeps{CityName: "seeded", CityPath: t.TempDir(), Config: &config.City{}, CityBeadStore: city})

	queue, ok := s.NudgeQueue().(nudgequeue.StateFileQueue)
	if !ok {
		t.Fatalf("NudgeQueue = %T, want nudgequeue.StateFileQueue", s.NudgeQueue())
	}
	if !beads.SameStore(queue.Shadows, city) {
		t.Errorf("the nudge queue's shadows are %v, want the city store", queue.Shadows)
	}

	if s.extmsgSvc == nil {
		t.Fatal("no external-messaging services over a city store")
	}
	group, err := s.extmsgSvc.Groups.EnsureGroup(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "tester"}, extmsg.EnsureGroupInput{
		RootConversation: extmsg.ConversationRef{
			ScopeID:        "city-1",
			Provider:       "discord",
			AccountID:      "acct-1",
			ConversationID: "thread-1",
			Kind:           extmsg.ConversationThread,
		},
		Mode: extmsg.GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := city.Get(group.ID); err != nil {
		t.Errorf("the city store does not hold group %q: %v", group.ID, err)
	}
}

func TestSeededStateWorkScopeStores(t *testing.T) {
	city, rig := beads.NewMemStore(), beads.NewMemStore()
	s := newSeededState(SeededCityDeps{CityName: "seeded", Config: &config.City{}, CityBeadStore: city, RigStores: map[string]beads.Store{"rig": rig}})
	for _, tc := range []struct {
		name string
		want beads.Store
	}{{"", city}, {"rig", rig}, {"missing", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			work, graph := s.WorkScopeStores(tc.name)
			if tc.want == nil {
				if !beads.IsNilStore(work) || !beads.IsNilStore(graph) {
					t.Fatal("unknown rig returned a store")
				}
				return
			}
			if !beads.SameStore(work, tc.want) || !beads.SameStore(graph, tc.want) {
				t.Fatal("work and graph handles do not point to the requested scope")
			}
		})
	}
}
