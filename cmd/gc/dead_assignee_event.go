package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// emitDeadAssigneeReopenedEvents records one event for each work bead
// releaseOrphanedPoolAssignments just reopened: bead.route_away_released for a
// claim released away from its live holder (RoutedAway), whose holder never
// died, and bead.dead_assignee_reopened for one whose assignee resolved to no
// open session bead. The destructive reopen (clear
// assignee, reset in_progress→open) already ran and is gated on confirmed
// non-liveness (snapshot-complete deferral + liveWorkAssignmentStillReleasable
// re-validation + liveOpenSessionAssignmentExists); this only makes the
// otherwise-silent repair observable, so it never mutates a bead.
//
// released carries the ID and the index into assignedWorkBeads (the pre-reopen
// snapshot) so the assignee and route can be read off the bead as it
// looked when it was reopened. A stale/out-of-range index is skipped rather
// than fabricating a payload.
func emitDeadAssigneeReopenedEvents(rec events.Recorder, assignedWorkBeads []beads.Bead, released []releasedPoolAssignment, now time.Time) {
	if rec == nil || len(released) == 0 {
		return
	}
	for _, r := range released {
		deadAssignee := ""
		routedTo := ""
		routedAt := ""
		if r.Index >= 0 && r.Index < len(assignedWorkBeads) && assignedWorkBeads[r.Index].ID == r.ID {
			wb := assignedWorkBeads[r.Index]
			deadAssignee = strings.TrimSpace(wb.Assignee)
			routedTo = strings.TrimSpace(wb.Metadata[beadmeta.RoutedToMetadataKey])
			routedAt = strings.TrimSpace(wb.Metadata[beadmeta.RoutedAtMetadataKey])
		}
		if r.RoutedAway {
			rec.Record(events.Event{
				Type:    events.BeadRouteAwayReleased,
				Ts:      now.UTC(),
				Actor:   "gc",
				Subject: r.ID,
				Message: formatRouteAwayReleasedMessage(r.ID, deadAssignee, routedTo),
				Payload: api.BeadRouteAwayReleasedPayloadJSON(r.ID, deadAssignee, routedTo, routedAt),
			})
			continue
		}
		rec.Record(events.Event{
			Type:    events.BeadDeadAssigneeReopened,
			Ts:      now.UTC(),
			Actor:   "gc",
			Subject: r.ID,
			Message: formatDeadAssigneeReopenedMessage(r.ID, deadAssignee, routedTo),
			Payload: api.BeadDeadAssigneeReopenedPayloadJSON(r.ID, deadAssignee, routedTo),
		})
	}
}

// formatRouteAwayReleasedMessage renders the operator-facing text for a
// bead.route_away_released event.
func formatRouteAwayReleasedMessage(beadID, holder, routedTo string) string {
	return "released routed work " + beadID + " from live session " + holder +
		" to route " + routedTo + "; assignee cleared so that pool can claim it"
}

// formatDeadAssigneeReopenedMessage renders the operator-facing text for a
// bead.dead_assignee_reopened event.
//
// deadAssignee == routedTo (both non-empty) is the bare-template-as-assignee
// shape: some routing paths write the template name itself into Assignee
// rather than a concrete session identity, so no session named after the
// template ever existed to die. The generic "assigned to dead session
// <template>" wording is misleading there, so that shape gets its own
// sentence (ga-r22k2y).
func formatDeadAssigneeReopenedMessage(beadID, deadAssignee, routedTo string) string {
	route := routedTo
	if route == "" {
		route = "<unrouted>"
	}
	if deadAssignee != "" && deadAssignee == routedTo {
		return "reopened routed work " + beadID + " routed to " + route +
			" with no live session claiming it; assignee cleared so the pool can reclaim it"
	}
	assignee := deadAssignee
	if assignee == "" {
		assignee = "<unknown>"
	}
	return "reopened routed work " + beadID + " assigned to dead session " + assignee +
		" (route " + route + "); assignee cleared so the pool can reclaim it"
}
