package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// emitDeadAssigneeReopenedEvents records one bead.dead_assignee_reopened event
// for each work bead releaseOrphanedPoolAssignments just reopened because its
// assignee resolved to no open session bead. The destructive reopen (clear
// assignee, reset in_progress→open) already ran and is gated on confirmed
// non-liveness (snapshot-complete deferral + liveWorkAssignmentStillReleasable
// re-validation + liveOpenSessionAssignmentExists); this only makes the
// otherwise-silent repair observable, so it never mutates a bead.
//
// released carries the ID and the index into assignedWorkBeads (the pre-reopen
// snapshot) so the dead assignee and routed_to can be read off the bead as it
// looked when it was reopened. A stale/out-of-range index is skipped rather
// than fabricating a payload.
func emitDeadAssigneeReopenedEvents(rec events.Recorder, assignedWorkBeads []beads.Bead, released []releasedPoolAssignment, now time.Time) {
	if rec == nil || len(released) == 0 {
		return
	}
	for _, r := range released {
		deadAssignee := ""
		routedTo := ""
		if r.Index >= 0 && r.Index < len(assignedWorkBeads) && assignedWorkBeads[r.Index].ID == r.ID {
			wb := assignedWorkBeads[r.Index]
			deadAssignee = strings.TrimSpace(wb.Assignee)
			routedTo = strings.TrimSpace(wb.Metadata[beadmeta.RoutedToMetadataKey])
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

// formatDeadAssigneeReopenedMessage renders the operator-facing text for a
// bead.dead_assignee_reopened event.
func formatDeadAssigneeReopenedMessage(beadID, deadAssignee, routedTo string) string {
	assignee := deadAssignee
	if assignee == "" {
		assignee = "<unknown>"
	}
	route := routedTo
	if route == "" {
		route = "<unrouted>"
	}
	return "reopened routed work " + beadID + " assigned to dead session " + assignee +
		" (route " + route + "); assignee cleared so the pool can reclaim it"
}
