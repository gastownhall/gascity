package main

import (
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// drainAckAnomalyReason is the shape discriminator a
// session.drain_acked_with_assigned_work payload carries for kept, the work
// bead row's finalized drain-ack stop kept assigned. It states mechanical
// facts of the stop, not a recovery decision; pack subscribers apply policy.
// In precedence order:
//
//   - restart_requested: the stop completed a restart request
//     (restartRequested), whose handoff keeps the row's work by design.
//   - started_claim_kept: kept is in_progress and the drain-ack release keeps
//     it as the row's started work (releaseSeat.keepsStarted: its started
//     records name it, or may, and it is on no dispatch hold).
//   - drain_acked_with_assigned_work: any other kept work, such as an open
//     preassigned row or an in_progress claim the row never started.
func drainAckAnomalyReason(row sessionpkg.Info, kept beads.Bead, restartRequested bool) string {
	if restartRequested {
		return api.DrainAckReasonRestartRequested
	}
	if kept.Status == "in_progress" && releaseSeatOfInfo(row).keepsStarted(kept) {
		return api.DrainAckReasonStartedClaimKept
	}
	return api.DrainAckReasonAssignedWork
}
