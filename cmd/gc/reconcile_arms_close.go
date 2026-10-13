package main

import (
	"cmp"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The closes of arm A21 (CONTRACT v5.7 C3, A21): the failed-create close
// (SESS-060/061), the pool-slot close with its stranded hold (SESS-623..626),
// a killed seat's included (B1), and the orphan close (SESS-079..083,
// SESS-722, MAINT-051). Each proposes the close effect, which confirms the
// stop (C8.8) and reads the work guard (L5) live before it closes. None
// fires on a partial pass, on Keep, or on an occupied, alive or unknown row:
// only on a row whose runtime is gone, is a corpse the row no longer claims
// as live (Q4), or, for an undesired row, is dead, a corpse or its own
// zombie, which the close removes first (MAINT-031, deadRuntimeClosable).

// A21's reasons.
const (
	decideCloseFailedCreate  = "close-failed-create"
	decideFailedCreateLeased = "close-held:pending-create-leased" // SESS-058
	decideClosePoolSlot      = "close-pool-slot"
	decideStrandedHold       = "stranded-hold"
	decideCloseOrphan        = "close-orphan"
)

// closeKind is which A21 close an intent is: its terminal patch, its
// cascade and its L5 branches.
type closeKind uint8

const (
	closeOrphan closeKind = iota + 1
	closeFailedCreate
	closePoolSlot
)

// closeSpec is a close intent's kind and what its L5 branches read: for a
// SESS-080 phantom the assignees it keeps (their work is the configured
// identity's, so L5 is skipped and the cascade releases none of it); an
// orphan drained as orphaned (SESS-082's release); a killed pool seat (B1's
// release, never the stranded branch); and a pool slot whose stranded marker
// has aged (SESS-625's repair).
type closeSpec struct {
	Kind     closeKind
	Phantom  bool
	Preserve []string
	Orphaned bool
	Killed   bool
	Repair   bool
	Template string
	// Row is the row the pass decided on, which the effect's work precheck
	// reads (closeWorthRunning); the effect itself decides on the fresh row.
	Row session.Info
}

// armClose is A21.
func armClose(r *rowFacts) (intent, bool) {
	if r.partial || r.entry == nil || !r.freshClosable() {
		return intent{}, false // no close on a partial pass (P-3)
	}
	info, e, now := r.row.Info, r.entry, r.w.Now
	it := intent{Kind: intentClose, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}}
	switch {
	case e.Desired == desireNone && e.Reason == reasonFailedCreate && r.failedCreateLeased():
		// SESS-058 (mc-mcud0): a failed create whose pending-create claim is
		// still leased stays open until the lease expires, as legacy keeps
		// it: a start may still hold it, and closing under it orphans its
		// runtime. A patrol reads the lease again.
		return intent{Reason: decideFailedCreateLeased}, true
	case e.Desired == desireNone && e.Reason == reasonFailedCreate:
		// The row ends: failedCreateClosePatch's lifecycle with the Retire
		// cell (the decider cannot mint; the close mints it).
		it.Reason, it.Patch = decideCloseFailedCreate, failedCreateCloseLife(now)
		it.Class = session.CellInfo(info, session.ActorController, session.DispEvent{Kind: session.EvRetire}, now)
		it.Closing = closeSpec{Kind: closeFailedCreate}
	case e.Desired == desireSleep && isPoolManagedSessionInfo(info) && !isNamedSessionInfo(info) && isPoolSessionSlotFreeableInfo(info, now):
		killed := strings.TrimSpace(info.SleepReason) == string(session.SleepReasonKilled)
		aged, held := r.strandedMarker()
		if held && !killed {
			return intent{Reason: decideStrandedHold}, true
		}
		it.Reason, it.Patch = decideClosePoolSlot, session.ClosePatch(now, cmp.Or(strings.TrimSpace(info.SleepReason), "drained"))
		it.Closing = closeSpec{Kind: closePoolSlot, Killed: killed, Repair: aged && !killed, Template: e.Template}
	case e.Desired == desireDrain && e.DrainReason != "" && !info.PendingCreateClaim && !deliberatelyHeldPoolRow(info, now):
		// An undesired pending create is A10's rollback, never a close. The
		// close reason is the drain's: orphaned, or suspended (§12.2 #17).
		// Its work is released as legacy's orphan close releases it: an
		// agent's suspend keeps only a configured named row's work for its
		// return, and legacy closes the rest as orphaned (SESS-082); a city
		// suspend keeps every row's.
		orphaned := e.DrainReason == drainOrphaned || !r.w.CitySuspended && !isNamedSessionInfo(info)
		it.Reason, it.Patch = decideCloseOrphan, session.ClosePatch(now, e.DrainReason)
		it.Closing = closeSpec{Kind: closeOrphan, Orphaned: orphaned, Template: e.Template}
		if keep, ok := r.w.Phantoms[r.k]; ok {
			// Both forms a claim on the configured identity can carry.
			it.Closing.Phantom, it.Closing.Preserve = true, keep
		}
	default:
		return intent{}, false
	}
	it.Closing.Row = info
	return it, true
}

// freshClosable reports that nothing live can hold the row's runtime, for
// A21: gone, or a corpse (no pane running) on a row that does not claim a
// live runtime; or, for an undesired row (deadRuntimeClosable), any dead
// runtime, which the close removes first (C5c2). It rests the decision on
// the runtime; on the effect's fresh read it is that read's class.
func (r *rowFacts) freshClosable() bool {
	r.rests |= restRuntime
	claims := sessionBeadClaimsLiveRuntime(r.row.Info)
	if r.fresh != nil {
		ownZombie := r.fresh.Class == rtZombie && ownRuntime(compareIdentity(r.row.Info, r.fresh.Identity), r.row.Info, r.fresh.Identity)
		dead := r.fresh.Class == rtCorpse || ownZombie // another row's zombie is a live pane (live-pane), its work never released
		return r.fresh.Class == rtAbsent || r.fresh.Class == rtCorpse && !claims || dead && r.deadRuntimeClosable()
	}
	switch r.entry.Liveness {
	case livenessGone:
		return true
	case livenessDead:
		name := strings.TrimSpace(r.row.Info.SessionName)
		return !claims && r.w.Obs.Fact(name, FactRunning, r.w.Now, r.w.ObsMaxAge).Value == ObsNo || r.deadRuntimeClosable()
	}
	return false
}

// deadRuntimeClosable is the dead-runtime close of an undesired row
// (MAINT-031; v5.7 A10, A21): a row AL1 drains whose runtime is dead, a
// corpse or a zombie, is closed, its dead runtime removed by exact object
// first, as legacy's cleanupDeadRuntimeSessionCorpses closes a row that
// claims the runtime it lost, keeping legacy's exclusions: a named row, a
// pending create (A10's rollback) and a row under a stop request (the stop
// verb's) are not, nor a row parked on a terminal provider error, which waits
// for its operator; a kill fence holds every arm before this one (A2). A row
// woken inside INC-003's grace waits it out, as legacy's drain begin does. A
// row the allocation wants awake restarts in place instead (C5a3's recycle).
func (r *rowFacts) deadRuntimeClosable() bool {
	return r.deadRuntimeCloses() && !r.inWakeGrace()
}

// deadRuntimeCloses is deadRuntimeClosable before the grace: a row A21
// closes, now or once its grace ends, which A12 leaves unclassified (a
// classification would park or quarantine a row legacy closes).
func (r *rowFacts) deadRuntimeCloses() bool {
	info := r.row.Info
	_, stopping := activeStop(r.row)
	return r.entry.Desired == desireDrain && r.entry.DrainReason != "" && !isNamedSessionInfo(info) && !info.PendingCreateClaim && !stopping &&
		!deliberatelyHeldPoolRow(info, r.w.Now) && // A21's orphan guard: a held slot is kept, and classified meanwhile
		!parkedTerminal(info) // a parked row waits for its operator (§12.2 #19, #59)
}

// inWakeGrace is INC-003's grace of an undesired row woken recently that no
// operator suspended, which requeues the row at its end.
func (r *rowFacts) inWakeGrace() bool {
	info := r.row.Info
	if !wakeGracePreservesUndesiredRow(info, r.w.Now) || r.w.OperatorSuspend[r.k] != "" {
		return false
	}
	woke, _ := parseRFC3339Metadata(info.LastWokeAt)
	r.after(requeueUntil(r.w.Now, woke.Add(wakeUndesiredGrace)))
	return true
}

// failedCreateLeased is legacy's pendingCreateSessionStillLeased on the row
// at the pass time (SESS-058): its claim's lease, or its start request, still
// active for an agent that is not suspended; never on a lease anchor skewed
// ahead of the clock (pendingCreateAnchorAhead, v5.9r), which would hold the
// row open for as long as the skew lasts.
func (r *rowFacts) failedCreateLeased() bool {
	var cfg *config.City
	if r.w.Env != nil {
		cfg = r.w.Env.Cfg
	}
	return pendingCreateSessionStillLeasedAt(r.row.Info, cfg, r.w.Now) && !pendingCreateAnchorAhead(r.row.Info, r.w.Now)
}

// deliberatelyHeldPoolRow is an undesired pool row a deliberate hold parks:
// a user hold, a wait hold, or a future or unreadable held_until or
// quarantined_until. A21 keeps its slot until the hold ends, stricter than
// legacy's MAINT-051 sweep, which closes it whatever holds it (§12.2 #27,
// v5.7 A10): Disposition.HoldsDeliberately.
func deliberatelyHeldPoolRow(info session.Info, now time.Time) bool {
	return isPoolManagedSessionInfo(info) && session.DispositionOfInfo(info).HoldsDeliberately(now)
}

// strandedMarker reads the stranded marker (SESS-624). held: it is still
// inside its window, whose end is then the row's deadline. aged: it is past
// the window, or more than the window in the future (skew never pins a
// row), and the close repairs. An empty or unparseable marker does neither
// (the C5c1 review's reversed ruling): the close is proposed, and L5 decides.
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
