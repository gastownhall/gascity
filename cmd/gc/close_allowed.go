package main

// The one close precondition (ARCH-RESTRUCTURE-2 §5.2 R8b, §5.3 R9 piece C,
// mc-zndi7.98). Every legacy close goes through closeSeat:
//
//  1. a work-gated close: the seat holds no open or in_progress work under
//     its ReleaseScope, on the tick's SeatWork read and again at the close
//     (lateSeatWork), the scope the close cascade releases on (SS-4);
//  2. the undesired pool row's close (closeFreeSlot) alone: the
//     row's Disposition.KeepsSlot is false (an operator's suspend keeps its
//     slot, §12.2 row 27), the one change from legacy it declares;
//  3. Store.CommitClose lands (legacy.close). Its premise refuses a wake or
//     restart request that lands after the decision; a request already on
//     the row does not refuse (nothing consumes one on an undesired row),
//     and D7 rule 4 stays the drain finalize's.
//
// A claim no lane serves never refuses a close (O5, revised): it stays on the
// closed row and the cascade raises session.stranded_claims. v2's close (A21)
// shares the pure part, closePermitted, and keeps its own transaction close
// as its CommitClose. The pending-create rollback
// (RollbackPendingCreateAtomically) keeps its own premise, the create lease.
// A refusal is logged once per episode, with its cause.

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// closeGate is which of the conditions a close takes.
type closeGate uint8

const (
	// closeGated is a close a gate decided on "this seat holds nothing":
	// conditions 1 and 3.
	closeGated closeGate = iota + 1
	// closeFreeSlot is the undesired pool row's close (MAINT-051's sweep,
	// gc_swept, and the reconciler's not-running undesired close of a pool
	// row): closeGated, and an operator's suspend keeps the slot.
	closeFreeSlot
	// closeReleasing is a close that releases the seat's work instead of
	// gating on it (a corpse, a stranded repair, a stale reap, a phantom, an
	// orphan's unserved claims): condition 3.
	closeReleasing
	// closeRollback is a failed create's close: condition 3, with the
	// failed-create terminal patch.
	closeRollback
)

// causeKeepsSlot refuses a free-slot close: an operator's suspend keeps the
// slot. causeHasWork is the drain's.
const causeKeepsSlot = "keeps-slot"

// closeWhy is why a legacy close closes: its reason, its gate, a gate's own
// work filter (the drain finalize excludes the seat's own drain step), and
// the assignees its cascade keeps (closeBeadPreservingAssignees).
type closeWhy struct {
	reason   string
	gate     closeGate
	keep     func(beads.Store, beads.Bead) (bool, error)
	preserve []string
}

// closePermitted is the pure part of the precondition, which v2's close
// shares: the cause refusing a close of info through gate, given whether a
// work-gated close's seat holds work, or "" when nothing refuses it.
func closePermitted(info sessionpkg.Info, gate closeGate, work bool) string {
	switch {
	case gate == closeFreeSlot && !isFailedCreateSessionInfo(info) && sessionpkg.DispositionOfInfo(info).KeepsSlot():
		return causeKeepsSlot
	case (gate == closeGated || gate == closeFreeSlot) && work:
		return causeHasWork
	}
	return ""
}

// closeAllowed is a legacy close's precondition short of its commit: the
// refusal's cause, or "" when the close may commit. A work-gated close reads
// sw, then the close-time re-read; a read that fails refuses (has-work, with
// its error).
func closeAllowed(sw *SeatWork, info sessionpkg.Info, why closeWhy) (string, error) {
	if cause := closePermitted(info, why.gate, false); cause != "" || why.gate != closeGated && why.gate != closeFreeSlot {
		return cause, nil
	}
	scope := releaseScope(info, sw.Legs().cfg)
	held, err := sw.has(seatWorkQuery{scope: scope, statuses: seatWorkStatuses, keep: why.keep})
	if err != nil || held {
		return causeHasWork, err
	}
	late, err := lateSeatWork(sw.Legs(), scope, seatWorkStatuses, why.keep)
	switch {
	case err != nil:
		return causeHasWork, fmt.Errorf("re-reading its work: %w", err)
	case len(late) > 0:
		return causeHasWork, fmt.Errorf("%s was assigned to it since this tick's read", late[0].bead.ID)
	}
	return "", nil
}

// closeSeat is every legacy close: closeAllowed, then the commit and its
// cascade (closeBeadPreservingAssignees), or, for a failed-create row a gate
// or a rollback closes, the failed-create close. It reports whether it
// closed.
func closeSeat(store beads.Store, sw *SeatWork, info sessionpkg.Info, why closeWhy, now time.Time, stderr io.Writer) bool {
	closed, _ := closeSeatFor(store, sw, info, why, now, stderr)
	return closed
}

// closeSeatFor is closeSeat with the refusal's cause, "" when the
// precondition allowed the close.
func closeSeatFor(store beads.Store, sw *SeatWork, info sessionpkg.Info, why closeWhy, now time.Time, stderr io.Writer) (bool, string) {
	if stderr == nil {
		stderr = io.Discard
	}
	cause, err := closeAllowed(sw, info, why)
	sw.closeRefusals().log(stderr, info.ID, cause, err)
	switch {
	case cause != "":
		return false, cause
	case why.gate == closeRollback, why.gate != closeReleasing && isFailedCreateSessionInfo(info):
		return closeFailedCreateBead(sessionFrontDoor(store), info, now, stderr), ""
	}
	return closeBeadPreservingAssignees(store, sw, info, why.reason, why.preserve, now, stderr), ""
}

// closeRefusalLog is each row's last logged close refusal, so a refusal is
// logged once per episode: when its cause first appears or changes, and again
// after a close was allowed. Its owner (the controller) keeps it across
// ticks; a nil log logs every refusal. A refusal written to no one (a nil or
// discarded writer) records nothing, so a caller that can print still does.
type closeRefusalLog struct {
	mu   sync.Mutex
	last map[string]string
}

// log logs id's refusal cause to w unless the episode already said it; an
// allowed close ("") ends the episode.
func (l *closeRefusalLog) log(w io.Writer, id, cause string, err error) {
	if l != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		switch {
		case cause == "":
			delete(l.last, id)
			return
		case l.last[id] == cause:
			return
		}
	}
	if cause == "" || w == io.Discard {
		return
	}
	if err != nil {
		fmt.Fprintf(w, "session beads: close of %s refused (%s): %v\n", id, cause, err) //nolint:errcheck
	} else {
		fmt.Fprintf(w, "session beads: close of %s refused (%s)\n", id, cause) //nolint:errcheck
	}
	if l != nil {
		if l.last == nil {
			l.last = map[string]string{}
		}
		l.last[id] = cause
	}
}
