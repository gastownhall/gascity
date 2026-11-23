package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// Kills over- and under-waking: TestRouterAllocatorWakePolicy's cases (C9)
// without the router's indexes. On any leg, replays included, a close or
// delete, a session row (relics too) and work whose payload carries
// gc.routed_to or an assignee are relevant; so is work the last pass read as
// demand, which stands for the router's "previously indexed" version. Any
// other work event is not, and neither is a closed route the last pass no
// longer read.
func TestBeadEventRelevantMatchesC9Policy(t *testing.T) {
	work := func(route, assignee string) beads.Bead { return routedWorkBead("w-1", "open", assignee, route) }
	read := relevantSet{"w-1": true}
	session := routerSessionBead("s-a", map[string]string{"session_name": "worker-a"})
	cases := []struct {
		name      string
		recent    relevantSet
		eventType string
		b         beads.Bead
		want      bool
	}{
		{name: "work with neither", eventType: events.BeadUpdated, b: work("", "")},
		{name: "work with neither, before any pass", recent: nil, eventType: events.BeadUpdated, b: work("", "")},
		{name: "work with neither, last pass read other work", recent: relevantSet{"w-2": true}, eventType: events.BeadUpdated, b: work("", "")},
		{name: "routed work", eventType: events.BeadCreated, b: work("pool", ""), want: true},
		{name: "assigned work", eventType: events.BeadUpdated, b: work("", "worker-a"), want: true},
		{name: "assigned blocked work", eventType: events.BeadUpdated, b: routedWorkBead("w-1", "blocked", "worker-a", ""), want: true},
		{name: "route or assignee removed from work the last pass read", recent: read, eventType: events.BeadUpdated, b: work("", ""), want: true},
		{name: "close of work with neither", eventType: events.BeadClosed, b: routedWorkBead("w-1", "closed", "", ""), want: true},
		{name: "delete of work with neither", eventType: events.BeadDeleted, b: work("", ""), want: true},
		{name: "session row", eventType: events.BeadUpdated, b: session, want: true},
		{name: "close of a session row carrying only its ID", eventType: events.BeadClosed, b: beads.Bead{ID: "s-a"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := beadEventRelevant(beadEvent(t, tc.eventType, tc.b), tc.recent); got != tc.want {
				t.Fatalf("beadEventRelevant = %v, want %v", got, tc.want)
			}
		})
	}
	if !beadEventRelevant(events.Event{Type: events.BeadUpdated, Payload: []byte("{")}, nil) {
		t.Fatal("an undecodable payload must be relevant")
	}
}

// Kills a missed or spurious inventory wake: a fact flip, a new name, a
// dropped name, a primed change and a health change are each a change; a
// republish of the same facts is not.
func TestInventoryChangedMatchesRouterPolicy(t *testing.T) {
	at := gatherNow
	obs := func(listed ObsFact) RuntimeObservation {
		return RuntimeObservation{Listed: RuntimeFact{Value: listed, ObservedAt: at}}
	}
	base := func() *ObservationSnapshot {
		return &ObservationSnapshot{
			ByName: map[string]RuntimeObservation{"s-1": obs(ObsYes)},
			Primed: map[string]bool{"tmux": true},
			Health: map[string]BackendHealth{"tmux": {State: backendHealthIdle}},
		}
	}
	cases := []struct {
		name string
		edit func(*ObservationSnapshot)
		want bool
	}{
		{name: "same facts, later observation", edit: func(s *ObservationSnapshot) {
			s.ByName["s-1"] = RuntimeObservation{Listed: RuntimeFact{Value: ObsYes, ObservedAt: at.Add(time.Second)}}
		}},
		{name: "fact flip", edit: func(s *ObservationSnapshot) { s.ByName["s-1"] = obs(ObsNo) }, want: true},
		{name: "new name", edit: func(s *ObservationSnapshot) { s.ByName["s-2"] = obs(ObsYes) }, want: true},
		{name: "dropped name", edit: func(s *ObservationSnapshot) { delete(s.ByName, "s-1") }, want: true},
		{name: "primed lost", edit: func(s *ObservationSnapshot) { s.Primed["tmux"] = false }, want: true},
		{name: "health change", edit: func(s *ObservationSnapshot) { s.Health["tmux"] = BackendHealth{State: "degraded"} }, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := base()
			tc.edit(next)
			if got := inventoryChanged(base(), next); got != tc.want {
				t.Fatalf("inventoryChanged = %v, want %v", got, tc.want)
			}
		})
	}
	if !inventoryChanged(nil, base()) || inventoryChanged(base(), nil) {
		t.Fatal("a first publish must be a change, and no publish must not be")
	}
}
