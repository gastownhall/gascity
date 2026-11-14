package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// drainDeadlineReason is the stop request A22 begins for a pool seat parked
// in an unfinalized drain past poolSlotDrainRetireDeadline: v2's counterpart
// of legacy's retirePoolSlotAtDrainDeadline (PAR-DRAINDL, ga-rxhu2).
//
// Why a drain of its own: a seat the drain-ack path left awake over a live
// runtime (sleep_reason=drained, drain_at stamped, last_woke_at cleared) is a
// seat the allocation still counts, whenever work is routed to its template.
// The allocation then keeps it, A20 finds no reason to drain it, and A4 and
// A21 have nothing to finish, so the seat holds its slot and its name for
// good. The bound does not decide the seat is unwanted; it ends a drain that
// outlived its deadline, through the ordinary signal, stop and finalize, and
// the work the seat's routed bead waits on goes to a new seat.
//
// No lens cancels it (drainReasonCancelable): demand for the seat is the
// wedge. Work assigned to the seat holds it instead (poolSlotRetireHasAssignedWork,
// read by the begin, the signal and the stop under the legacy identity set).
// There is no force path (PAR-FORCEKILL): a runtime that will not stop keeps
// its row open, and D6 raises session.drain_stop_escalated.
const drainDeadlineReason = "pool-slot-drain-deadline"

// drainDeadlineDue is the bound's decision on the row, shared with legacy
// (poolSlotRetireDue): the seat is a pool seat in an unfinalized drain older
// than the deadline, and its runtime is alive.
func (r *rowFacts) drainDeadlineDue() bool {
	if r.entry == nil || r.entry.Liveness != livenessAlive {
		return false
	}
	info := r.row.Info
	_, due := poolSlotRetireDue(info, worldCfg(r.w), poolSlotRetireTemplate(info, ""), r.w.Now)
	return due
}

// armDrainDeadline is A22: a seat past the pool-slot drain deadline with no
// stop request gets the deadline's, by CAS. Its begin stamps the request with
// the drain_at the seat entered drain at (not now), so the drain's true age
// survives the signal, which restarts drain_at for D6, and the signal is due
// at once. Placed after A21, every arm that finalizes a drain on its own (the
// ordinary chain) has had its turn.
func armDrainDeadline(r *rowFacts) (intent, bool) {
	if _, active := activeStop(r.row); active {
		return intent{}, false
	}
	if !r.drainDeadlineDue() {
		return intent{}, false
	}
	it, ok := r.drainBegin(drainDeadlineReason, nil)
	if drainAt := strings.TrimSpace(r.row.Info.DrainAt); drainAt != "" {
		it.Patch[session.DrainIntentAtKey] = drainAt
	}
	return it, ok
}
