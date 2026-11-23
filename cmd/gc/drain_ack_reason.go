package main

import (
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// drainAckAnomalyReason names the shape of the work a drain-ack finalize
// kept, for the session.drain_acked_with_assigned_work payload's reserved
// reason (gastownhall/gascity#6940). It is a mechanical fact about the row
// and the kept bead, not recovery policy: what a subscriber does with the
// shape stays in the pack.
//
// A kept in_progress claim the row's own started records name, on no
// dispatch hold — the claim the drain-ack release keeps and the OD9 fence
// refuses a seat's own ack over — is started_claim_kept: the shape a
// claim-refuse-escalate cycle repeats. Every other kept bead keeps the
// generic drain_acked_with_assigned_work: an open preassigned row, an
// in_progress claim the row never started (a live sibling's), or a started
// claim on a dispatch hold, which the release gives back as it always has.
func drainAckAnomalyReason(row session.Info, kept beads.Bead) string {
	if kept.Status == "in_progress" && releaseSeatOfInfo(row).keepsStarted(kept) {
		return api.DrainAckReasonStartedClaimKept
	}
	return api.DrainAckReasonAssignedWork
}
