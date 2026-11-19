package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// A tick that releases one claim routed away from its live holder and one
// claim of a dead assignee records exactly one event per bead: the route-away
// bead gets bead.route_away_released (its holder lives, so it was never a dead
// assignee), and the dead-assignee bead gets only bead.dead_assignee_reopened.
func TestRouteAwayReleaseRecordsItsOwnEvent(t *testing.T) {
	routedAt := beadmeta.FormatRoutedAt(time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC))
	assigned := []beads.Bead{
		{ID: "w-1", Assignee: "alpha-1", Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "beta",
			beadmeta.RoutedAtMetadataKey: routedAt,
		}},
		{ID: "w-2", Assignee: "beta-dead", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "beta"}},
	}
	released := []releasedPoolAssignment{{ID: "w-1", Index: 0, RoutedAway: true}, {ID: "w-2", Index: 1}}
	rec := &capturingRecorder{}

	emitDeadAssigneeReopenedEvents(rec, assigned, released, time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))

	if len(rec.events) != 2 {
		t.Fatalf("events = %+v, want one per released bead", rec.events)
	}
	byType := map[string][]events.Event{}
	for _, e := range rec.events {
		byType[e.Type] = append(byType[e.Type], e)
	}
	routeAway := byType[events.BeadRouteAwayReleased]
	if len(routeAway) != 1 || routeAway[0].Subject != "w-1" {
		t.Fatalf("%s events = %+v, want one for w-1", events.BeadRouteAwayReleased, routeAway)
	}
	var p api.BeadRouteAwayReleasedPayload
	if err := json.Unmarshal(routeAway[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	want := api.BeadRouteAwayReleasedPayload{BeadID: "w-1", Holder: "alpha-1", RoutedTo: "beta", RoutedAt: routedAt}
	if p != want {
		t.Fatalf("payload = %+v, want %+v", p, want)
	}
	dead := byType[events.BeadDeadAssigneeReopened]
	if len(dead) != 1 || dead[0].Subject != "w-2" {
		t.Fatalf("%s events = %+v, want only one, for w-2", events.BeadDeadAssigneeReopened, dead)
	}
}
