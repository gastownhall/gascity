package main

// The one close precondition (ARCH-RESTRUCTURE-2 §5.2 R8b, §5.3 R9 piece C,
// mc-zndi7.98). Every legacy close goes through closeSeat:
//
//  1. the row's Disposition.KeepsSlot is false (an operator's suspend keeps
//     its slot, §12.2 row 27);
//  2. a work-gated close: no pending request the controller honors (an
//     explicit wake_request, restart_requested);
//  3. a work-gated close: the seat holds no open or in_progress work under
//     its ReleaseScope, on the tick's SeatWork read and again at the close
//     (lateSeatWork), the scope the close cascade releases on (SS-4);
//  4. Store.CommitClose lands (legacy.close).
//
// A failed-create row takes neither 1 nor 2: legacy closed it whatever held
// it. A claim no lane serves never refuses a close (O5, revised): it stays
// on the closed row and the cascade raises session.stranded_claims. v2's
// close (A21) shares the pure part, closePermitted, and keeps its own
// transaction close as its CommitClose. The pending-create rollback
// (RollbackPendingCreateAtomically) keeps its own premise, the create lease.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// closeGate is which of the conditions a close takes.
type closeGate uint8

const (
	// closeGated is a close a gate decided on "this seat holds nothing":
	// all four conditions.
	closeGated closeGate = iota + 1
	// closeDuplicate is closeGated without the request condition: legacy's
	// duplicate close never read one, and a stale wake request must not keep
	// a duplicate seat open.
	closeDuplicate
	// closeReleasing is a close that releases the seat's work instead of
	// gating on it (a corpse, a stranded repair, a stale reap, a phantom, an
	// orphan's unserved claims): conditions 1 and 4.
	closeReleasing
	// closeRollback is the failed create of a row this pass created:
	// condition 4 alone.
	closeRollback
)

// Close refusal causes; causeHasWork is the drain's.
const (
	causeKeepsSlot      = "keeps-slot"      // an operator's suspend keeps the slot
	causeRequestPending = "request-pending" // a wake or restart request the controller honors
)

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
	failed := isFailedCreateSessionInfo(info)
	switch {
	case gate == closeRollback:
		return ""
	case !failed && sessionpkg.DispositionOfInfo(info).KeepsSlot():
		return causeKeepsSlot
	case gate == closeReleasing:
		return ""
	case gate == closeGated && !failed && closeRequestPending(info):
		return causeRequestPending
	case work:
		return causeHasWork
	}
	return ""
}

// closeRequestPending reports a request the controller honors: an explicit
// wake request or a restart request.
func closeRequestPending(info sessionpkg.Info) bool {
	return strings.TrimSpace(info.WakeRequest) == string(sessionpkg.WakeCauseExplicit) ||
		strings.TrimSpace(info.RestartRequested) == "true"
}

// closeAllowed is a legacy close's precondition short of its commit: the
// refusal's cause, or "" when the close may commit. A work-gated close reads
// sw, then the close-time re-read; a read that fails refuses (has-work, with
// its error).
func closeAllowed(sw *SeatWork, info sessionpkg.Info, why closeWhy) (string, error) {
	if cause := closePermitted(info, why.gate, false); cause != "" || why.gate == closeReleasing || why.gate == closeRollback {
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
	if stderr == nil {
		stderr = io.Discard
	}
	cause, err := closeAllowed(sw, info, why)
	switch {
	case cause != "" && err != nil:
		fmt.Fprintf(stderr, "session beads: close of %s refused (%s): %v\n", info.ID, cause, err) //nolint:errcheck
		return false
	case cause != "":
		return false
	case why.gate == closeRollback, why.gate != closeReleasing && isFailedCreateSessionInfo(info):
		return closeFailedCreateBead(sessionFrontDoor(store), info, now, stderr)
	}
	return closeBeadPreservingAssignees(store, sw, info, why.reason, why.preserve, now, stderr)
}
