package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// SESS-612's fresh-mode cycle (CONTRACT v5 §4 A13, v5.10q; plan C7a-2c),
// legacy's cycleAliveSessionForFreshReassign (session_bead_cycle.go): an
// alive wake_mode=fresh row the allocation reassigned to a new bead (its
// assigned work is not the bead it is processing, RequiresFreshCycle), which
// has not claimed that bead itself, is stopped and handed off as a restart
// onto the new bead, so its next start is a fresh conversation. The stop
// defers, in the effect, while the previous bead is still open, or when this
// incarnation woke after that bead closed (it is already fresh); the
// lookup's failure defers too, as a failed read proves nothing (legacy
// fails toward the cycle). A defer refuses on the cycle's own backoff key
// (freshCycleBackoffKey), which holds no other intent of the row, and
// writes nothing (reconcile_effect_fresh_cycle.go).

// decideFreshCycle is the fresh cycle's stop reason.
const decideFreshCycle = "fresh-cycle"

// freshCycle is A13's SESS-612 row: a Wake row, alive and its own, in fresh
// wake mode, whose assigned work needs a fresh cycle and which has not
// claimed that work itself, gets the stop verb with the fresh-cycle
// finalize (freshCycleMintKey, freshCycleClearKey).
func (r *rowFacts) freshCycle() (intent, bool) {
	e, info := r.entry, r.row.Info
	if e == nil || e.Desired != desireWake || e.AssignedWork == nil || !e.AssignedWork.RequiresFreshCycle || info.WakeMode != "fresh" {
		return intent{}, false
	}
	bead := strings.TrimSpace(e.AssignedWork.BeadID)
	if bead == "" || strings.TrimSpace(info.CurrentClaimBeadID) == bead || operatorDormantStop(info, r.w.Now) {
		return intent{}, false
	}
	// A deferred or refused cycle backs off its own key (the #7879 review):
	// while that record is live the row falls through to its later arms,
	// and is decided again when it releases.
	if rec, ok := r.w.Backoff[freshCycleBackoffKey(r.k)]; ok && rec.live(r.w.Now) {
		r.after(rec.Until.Sub(r.w.Now))
		return intent{}, false
	}
	if !r.freshOwnAlive() {
		return intent{}, false
	}
	res, _ := r.w.Templates.lookup(info)
	it := intent{Kind: intentStop, Reason: decideFreshCycle, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}, Finish: freshCycleClearKey}
	if restartKeyMints(res.TP, info) {
		it.Finish = freshCycleMintKey
	}
	return it, true
}

// freshCycleMintKey is the fresh cycle's finalize for a provider that takes
// a session id: the restart handoff with a new key, anchored to the new
// bead.
func freshCycleMintKey(f finalizeFacts) finalizeChoice {
	c := restartHandoffMintKey(f)
	anchorFreshCycle(c.Patch, f)
	return c
}

// freshCycleClearKey is the fresh cycle's finalize for any other provider.
func freshCycleClearKey(f finalizeFacts) finalizeChoice {
	c := restartHandoffClearKey(f)
	anchorFreshCycle(c.Patch, f)
	return c
}

// anchorFreshCycle sets the row's current bead to the new one in the
// handoff, so the next pass's divergence check does not cycle it again.
func anchorFreshCycle(patch session.MetadataPatch, f finalizeFacts) {
	if f.AssignedBead != "" {
		patch[session.CurrentBeadIDKey] = f.AssignedBead
	}
}
