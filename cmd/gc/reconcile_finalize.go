package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// finalizeFacts are what a stopped drain's finalize decides on (CONTRACT v5
// D4).
type finalizeFacts struct {
	Now time.Time
	// CloseIfUnassigned is the caller's: a seat a drained finalize may
	// close.
	CloseIfUnassigned bool
	// HasWork is the row's assigned work in the close-gate form, read live
	// (sessionHasOpenAssignedWorkForReachableStoreForCloseGate); an error
	// counts as work.
	HasWork bool
	// OperatorHeld is an operator's hold (Disposition.KeepsSlot): the seat is kept.
	OperatorHeld bool
	// HonoredWake is a pending explicit wake AL1 honors (D7 rule 4): no
	// close lands over it. Legacy has none.
	HonoredWake      bool
	FreshWake        bool // wake_mode=fresh
	RestartRequested bool
	// AssignedBead is the work the allocation assigns the row, which
	// SESS-612's fresh cycle anchors the row to.
	AssignedBead string
}

// stopFinish builds a stopped row's finalize from its facts.
type stopFinish func(finalizeFacts) finalizeChoice

// finalizeChoice is a finalize's write: the premise close, or Patch.
type finalizeChoice struct {
	CloseSeat bool
	// Patch is the non-close write, also the one a close its gate refused
	// falls back to (with HasWork read again).
	Patch session.MetadataPatch
	// Sleep marks the drain that keeps work, which completes as the
	// disposition's sleep (ApplySleep); a bare drain-ack is no disposition
	// move (ApplyLifecycle).
	AsSleep bool
}

// finalizePatch is the one finalize builder (CONTRACT v5 D4; DS F2),
// extracted from finalizeDrainAckStoppedSession, which calls it too: a seat
// that may close, holds no work and no operator hold, and has no honored
// wake closes; otherwise AcknowledgeDrainPatch (state=drained) with no work,
// or CompleteDrainPatch(idle) with work. Either clears restart_requested
// (#2574). Over an honored wake it leaves slept_at as it was: a sleep stamped
// after the request reads it served (explicitWakePendingInfo), and D7 rule 4
// leaves the request to the next pass's start (v5.10n). The stop request's
// keys are the stop verb's to clear, in the same CAS (D3 step 5).
func finalizePatch(f finalizeFacts) finalizeChoice {
	c := finalizeChoice{CloseSeat: f.CloseIfUnassigned && !f.HasWork && !f.OperatorHeld && !f.HonoredWake, AsSleep: f.HasWork}
	c.Patch = session.AcknowledgeDrainPatch(f.Now, f.FreshWake)
	if f.HasWork {
		c.Patch = session.CompleteDrainPatch(f.Now, string(session.SleepReasonIdle), f.FreshWake)
	}
	if f.HonoredWake {
		delete(c.Patch, "slept_at")
	}
	if f.RestartRequested {
		c.Patch["restart_requested"] = ""
	}
	// Durably mark (or clear) the stranded-holder fact alongside the sleep the
	// same patch writes (#5731). session.drain_acked_with_assigned_work already
	// reports this moment, but an event is not queryable state and the patch is
	// an ordinary idle sleep — so on the bead a stranded holder is
	// indistinguishable from a seat that merely went idle. The orphan-release
	// lane needs that distinction to tell a live claimant from a seat that
	// acknowledged its own exit while holding the claim (see
	// releaseOrphanedPoolAssignments). Nothing about the work bead changes here:
	// recovery stays out of the drain-ack path (#2293). f.Now is the patch's
	// single clock read, so slept_at and drain_ack_stranded_at cannot disagree
	// by a tick. A bare drain-ack, a no-work finalize, and an honored wake
	// (the seat starts; it never slept stranded) all clear it, so the marker
	// can never go stale; PreWakePatch clears it again when the seat starts.
	if f.HasWork && !f.HonoredWake {
		c.Patch[session.DrainAckStrandedAtKey] = f.Now.UTC().Format(time.RFC3339)
	} else {
		c.Patch[session.DrainAckStrandedAtKey] = ""
	}
	return c
}
