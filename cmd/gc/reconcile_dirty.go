package main

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The planner's dirty filter (CONTRACT v5 P1): which store notifications and
// inventory publishes are worth a pass. Every trigger is a hint (R-6): a
// notification filtered out here reaches the planner by the patrol pass. It
// is the router's allocator wake policy (C9) without the router's indexes;
// the "previously indexed" work is the relevant set the last pass published.

// relevantSet is the work the last pass read as demand: assigned work and
// open unassigned routed work, by bead ID. The pass publishes a fresh one,
// never mutated after publish, so a filter on another goroutine may read the
// one it loaded.
type relevantSet map[string]bool

// newRelevantSet is the relevant set of v's work.
//
//gc:pure
func newRelevantSet(v demandView) *relevantSet {
	s := make(relevantSet, len(v.AssignedWork)+len(v.Collected.UnassignedRouted))
	for _, b := range append(slices.Clip(v.AssignedWork), v.Collected.UnassignedRouted...) {
		s[b.ID] = true
	}
	return &s
}

// beadEventRelevant reports whether evt, on any leg and replays included, is
// worth a pass: a session row (relics too, the create fence); a close or
// delete; work whose payload carries gc.routed_to or an assignee; or work the
// last pass read as demand (recent), which covers an unroute or an unassign.
// An undecodable payload is relevant. Any other work event waits for the
// patrol pass.
//
//gc:pure
func beadEventRelevant(evt events.Event, recent relevantSet) bool {
	b, ok := beads.DecodeBeadEventPayload(evt.Payload)
	if !ok {
		return true
	}
	return session.IsSessionBeadOrRepairable(b) ||
		evt.Type == events.BeadClosed || evt.Type == events.BeadDeleted ||
		strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" || strings.TrimSpace(b.Assignee) != "" ||
		recent[b.ID]
}

// inventoryChanged reports whether an inventory publish changed anything a
// pass reads: a name's facts, incarnation or owner, a name the cache stopped
// tracking, a backend's primed bit or its health. The cache already applies
// the partial-list rule, so a partial pass yields no absence flip (R14).
//
//gc:pure
func inventoryChanged(prev, next *ObservationSnapshot) bool {
	if next == nil {
		return false
	}
	if prev == nil {
		prev = &ObservationSnapshot{}
	}
	for name, obs := range next.ByName {
		if old, had := prev.ByName[name]; observationChanged(old, obs, had) {
			return true
		}
	}
	for name := range prev.ByName {
		if _, ok := next.ByName[name]; !ok {
			return true
		}
	}
	return primedChanged(prev.Primed, next.Primed) || healthChanged(prev.Health, next.Health)
}

//
//gc:pure
func healthChanged(prev, next map[string]BackendHealth) bool {
	if len(prev) != len(next) {
		return true
	}
	for label, h := range next {
		if p, ok := prev[label]; !ok || p.State != h.State {
			return true
		}
	}
	return false
}
