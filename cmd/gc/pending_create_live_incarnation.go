package main

import (
	"fmt"
	"io"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// liveIncarnationHoldKind is the standing condition of a legacy tick's
// live-incarnation hold.
const liveIncarnationHoldKind = "live_incarnation.pending_create_rollback"

// holdsOverLiveIncarnation is the legacy tick's hold of a pending create
// whose name a live earlier or later incarnation of the row's own session
// holds (identity.otherEpochOfThisSession, #2081). The rollback would stop
// that runtime under a row-owned name, or close the row under it, so the
// tick holds while the session has in-progress work, the incarnation's in
// flight, or while that work cannot be read: it reports true, says so once
// per episode, and traces the hold every tick under a reason that arms no
// auto-trace. Open work is no incarnation's in flight: one the row no longer
// names cannot claim it (gc hook --claim refuses its token), and its own
// drain-ack gives back the claims it holds (refused while it holds started
// work). So once no in-progress work is left it reports false, and the
// rollback takes the row and its runtime as it takes any other
// incarnation's, releasing the open work to the session's next incarnation.
func holdsOverLiveIncarnation(sw *SeatWork, info sessionpkg.Info, identity pendingCreateIdentity, name, template string, dt *drainTracker, stderr io.Writer, trace *sessionReconcilerTraceCycle, now time.Time) bool {
	working, err := liveIncarnationWorking(sw, info)
	if err == nil && !working {
		return false
	}
	why := "its work is in progress"
	if err != nil {
		why = fmt.Sprintf("its work cannot be read (%v)", err)
	}
	logStandingCondition(dt, stderr, info.ID, liveIncarnationHoldKind, fmt.Sprintf(
		"session reconciler: pending-create rollback of %s deferred: a live runtime of this session (%s) holds the name and %s (live_incarnation)", name, liveIncarnationDetail(info, identity), why), now)
	if trace != nil {
		trace.RecordDecision(TraceSiteReconcilerPendingCreate, TraceReasonPendingCreateLiveIncarnation, TraceOutcomeDeferred, template, name, traceRecordPayload{
			"session_bead_id": info.ID,
			"attribution":     "live_incarnation",
		})
	}
	return true
}

// liveIncarnationWorking reports in-progress work assigned to info's
// session: the in-flight work of a live incarnation of it (#2081). No such
// work releases the hold, and the rollback then stops that live runtime or
// closes the row over it, so the question is a kill gate's (O4): read live,
// under the seat's RefuseScope (refuseScope), which counts work claimed
// under its alias (gc hook --claim is alias-first) or its alias history, not
// under its ReleaseScope alone.
func liveIncarnationWorking(sw *SeatWork, info sessionpkg.Info) (bool, error) {
	return sessionHasAssignedWorkInStoresForStatuses(sw, refuseScope(info, sw.Legs().cfg), []string{"in_progress"})
}
