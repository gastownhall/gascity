package main

import (
	"cmp"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The closes of arm A21 (CONTRACT v5.6 C3, A21): the failed-create close
// (SESS-060/061), the pool-slot close with its stranded marker and repair
// (SESS-623..626), and the orphan close (SESS-079..083, SESS-722,
// MAINT-051). Each proposes the close effect, which confirms the stop
// (C8.8) and reads the work guard (L5) live before it closes. None fires on
// a partial pass, on Keep, on a zombie, or on an occupied, alive or unknown
// row: only on a row
// whose runtime is gone, or is a corpse the row no longer claims as live
// (MAINT-031's corpse of a live-claiming row is C5c2's).

// A21's reasons.
const (
	decideCloseFailedCreate = "close-failed-create"
	decideClosePoolSlot     = "close-pool-slot"
	decideStrandedHold      = "stranded-hold"
	decideCloseOrphan       = "close-orphan"
)

// armClose is A21's failed-create, pool-slot and orphan closes.
func armClose(r *rowFacts) (intent, bool) {
	if r.partial || !closableRuntime(r) {
		return intent{}, false // no close on a partial pass (P-3)
	}
	info, e := r.row.Info, r.entry
	it := intent{Kind: intentClose, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}}
	switch {
	case e.Desired == desireNone && e.Reason == reasonFailedCreate:
		it.Reason, it.Patch = decideCloseFailedCreate, failedCreateClosePatch(r.w.Now)
		it.Closing = closeSpec{Kind: closeFailedCreate}
	case e.Desired == desireSleep && isPoolSessionSlotFreeableInfo(info) && isPoolManagedSessionInfo(info) && !isNamedSessionInfo(info):
		aged, held := r.strandedMarker()
		if held {
			return intent{Reason: decideStrandedHold}, true
		}
		it.Reason, it.Patch = decideClosePoolSlot, session.ClosePatch(r.w.Now, cmp.Or(strings.TrimSpace(info.SleepReason), "drained"))
		it.Closing = closeSpec{Kind: closePoolSlot, Repair: aged, Template: e.Template}
	case e.Desired == desireDrain && e.DrainReason != "" && !info.PendingCreateClaim:
		// An undesired pending create is A10's rollback, never a close.
		it.Reason, it.Patch = decideCloseOrphan, session.ClosePatch(r.w.Now, e.DrainReason)
		it.Closing = closeSpec{Kind: closeReleasing, Orphaned: e.DrainReason == drainOrphaned}
		if r.w.Env == nil || r.w.Env.Cfg == nil {
			break
		}
		if identity, ok := recyclableDeadConfiguredNamePhantomInfo(info, r.w.Env.Cfg, r.w.CityName); ok {
			// The work belongs to the configured identity, not this bead:
			// keep both forms a claim on it can carry (SESS-080).
			it.Closing.Phantom, it.Closing.Preserve = true, []string{identity}
			if rn := config.NamedSessionRuntimeName(r.w.CityName, r.w.Env.Cfg.Workspace, identity); rn != "" && rn != identity {
				it.Closing.Preserve = append(it.Closing.Preserve, rn)
			}
		}
	default:
		return intent{}, false
	}
	return it, true
}

// closableRuntime reports whether nothing live can hold the row's runtime:
// the inventory reads it gone, or reads a corpse (a dead pane, not only a
// dead agent) on a row that no longer claims a live runtime.
func closableRuntime(r *rowFacts) bool {
	switch r.entry.Liveness {
	case livenessGone:
		return true
	case livenessDead:
		return r.w.Observed[r.k].Corpse && !sessionBeadClaimsLiveRuntime(r.row.Info)
	}
	return false
}

// strandedMarker reads the stranded marker the close effect stamps on a
// pool slot that still holds work (SESS-624; v5.6 C3). aged: the episode
// passed strandedRepairConfirmGrace, or the marker is more than that far in
// the future (skew never pins a row), and the effect repairs it. held: the
// window still runs, and its end is the row's deadline. An empty or
// unparseable marker neither ages nor holds: the plain close is proposed,
// and its L5 decides (only the repair waits on a marker).
func (r *rowFacts) strandedMarker() (aged, held bool) {
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(r.row.Info.StrandedEventEmittedAt))
	switch {
	case err != nil:
		return false, false
	case r.w.Now.Sub(at) >= strandedRepairConfirmGrace || at.Sub(r.w.Now) > strandedRepairConfirmGrace:
		return true, false
	}
	r.after(at.Add(strandedRepairConfirmGrace).Sub(r.w.Now))
	return false, true
}
