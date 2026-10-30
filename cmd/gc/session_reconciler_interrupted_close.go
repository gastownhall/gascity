package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Interrupted closes (#2085). A store without an atomic terminal close commits
// a session close as two writes, the ClosePatch metadata and then the status
// Close. When the Close is lost (the controller stopped between the two
// writes, the projection wait failed, or the Close itself failed) the bead
// stays open with this binary's own close code as its state. That code is not
// a known state. Most closes that write one are gated on the row's prior
// state and never run again, and the rest (duplicate, reconfigured, gc_swept)
// run again only while their trigger holds, so the row could hold its pool or
// named identity forever. The reconciler finishes such a close before the
// unknown-state skip, or rolls it back when its gate now refuses it.

// interruptedCloseGates maps each close code this binary writes, and that is
// not itself a known state, to the gate of the close that writes it.
var interruptedCloseGates = map[string]closeGate{
	"duplicate":               closeGated,
	"reconfigured":            closeGated,
	"gc_swept":                closeFreeSlot,
	"stale-session":           closeReleasing,
	"dead-runtime":            closeReleasing,
	strandedRepairCloseReason: closeReleasing,
}

// interruptedClosePassthroughCodes are the pool-freeable sleep reasons the
// not-running pool close writes verbatim as its close code
// (ClosePatch(now, TrimSpace(sleep_reason))) that are not known states.
// TestInterruptedCloseCensusCoversEveryCloseCode keeps it complete.
var interruptedClosePassthroughCodes = map[string]bool{
	string(sessionpkg.SleepReasonIdle):                  true,
	string(sessionpkg.SleepReasonIdleTimeout):           true,
	string(sessionpkg.SleepReasonCityStop):              true,
	string(sessionpkg.SleepReasonRuntimeMissing):        true,
	string(sessionpkg.SleepReasonProviderTerminalError): true,
	string(sessionpkg.SleepReasonMaxSessionAge):         true,
	string(sessionpkg.SleepReasonConfigDrift):           true,
	string(sessionpkg.SleepReasonKilled):                true,
}

// interruptedCloseGate reports whether info is an interrupted close of this
// binary's own, and the gate of the close that wrote it. It matches the raw,
// untrimmed state, as isKnownStateInfo does. A passthrough code is recognized
// only in the shape its close writes: the state equals the row's own trimmed
// sleep_reason, on a pool-managed row that is not a configured named session.
func interruptedCloseGate(info sessionpkg.Info) (closeGate, bool) {
	state := info.MetadataState
	if gate, ok := interruptedCloseGates[state]; ok {
		return gate, true
	}
	if interruptedClosePassthroughCodes[state] &&
		state == strings.TrimSpace(info.SleepReason) &&
		isPoolManagedSessionInfo(info) &&
		!isNamedSessionInfo(info) {
		return closeGated, true
	}
	return 0, false
}

// finishInterruptedClose finishes info's interrupted close through gate and
// reports whether the row closed. It takes the same steps as the failed-create
// close: no close on a partial store view or a boot pass, no close unless the
// runtime is confirmed not running, release of the row's own runtime lease,
// then closeSeat with the interrupted close's own code and gate, which rewrites
// the same ClosePatch and runs the same guards and cascade. A commit that does
// not land keeps the close code, and a later tick finishes it. When the gate
// itself refuses (work reached the seat after the metadata half landed, or an
// operator's suspend keeps its slot), the close no longer applies: the row is
// rolled back to asleep (rollBackInterruptedClose) and fold is the rollback's
// patch. Any other refusal leaves the row to the unknown-state skip. Each
// decision is recorded on trace at TraceSiteReconcilerFinishInterruptedClose,
// and a finished or rolled-back close also writes a WARN line to stderr.
func finishInterruptedClose(
	cityPath string,
	store beads.Store,
	sw *SeatWork,
	sp runtime.Provider,
	cfg *config.City,
	dt *drainTracker,
	info sessionpkg.Info,
	gate closeGate,
	storeQueryPartial, deferOnBoot bool,
	trace *sessionReconcilerTraceCycle,
	clk clock.Clock,
	stderr io.Writer,
) (fold sessionpkg.MetadataPatch, closed bool) {
	if storeQueryPartial || deferOnBoot {
		recordInterruptedClose(trace, info, TraceOutcomeSkipped, traceRecordPayload{
			"store_query_partial":          storeQueryPartial,
			"defer_session_closes_on_boot": deferOnBoot,
		})
		return nil, false
	}
	alive, err := workerSessionTargetRunningWithConfig(cityPath, store, sp, cfg, info.ID)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: skipping interrupted close of '%s': liveness observation failed: %v\n", info.SessionNameMetadata, err) //nolint:errcheck
		recordInterruptedClose(trace, info, TraceOutcomeSkippedLivenessError, traceRecordPayload{
			"liveness_error": err.Error(),
		})
		return nil, false
	}
	if alive {
		recordInterruptedClose(trace, info, TraceOutcomeKeptOpen, traceRecordPayload{"hold": "runtime_alive"})
		return nil, false
	}
	if !releaseRowOwnedRuntimeLeased(cityPath, store, info, sp, stderr, standingRowRuntimeHold(dt, stderr, info.ID, clk.Now())) {
		recordInterruptedClose(trace, info, TraceOutcomeKeptOpen, traceRecordPayload{"hold": "runtime_teardown"})
		return nil, false
	}
	closed, cause := closeSeatFor(store, sw, info, closeWhy{reason: info.MetadataState, gate: gate}, clk.Now().UTC(), stderr)
	switch {
	case !closed && cause == "":
		recordInterruptedClose(trace, info, TraceOutcomeNoChange, traceRecordPayload{"cause": "commit_not_landed"})
		return nil, false
	case !closed:
		return rollBackInterruptedClose(store, info, cause, trace, clk.Now(), stderr), false
	}
	if info.MetadataState == strandedRepairCloseReason || interruptedClosePassthroughCodes[info.MetadataState] {
		pruneAgentHomeWorktreeIfSafeInfo(info, cityPath, cfg, stderr)
	}
	fmt.Fprintf(stderr, "WARN: reconciler: finished interrupted %q close of session bead %s — runtime not running\n", info.MetadataState, info.ID) //nolint:errcheck
	recordInterruptedClose(trace, info, TraceOutcomeClosed, traceRecordPayload{
		"state": info.MetadataState,
		"gate":  closeGateName(gate),
	})
	return nil, true
}

// rollBackInterruptedClose undoes the metadata half of info's interrupted
// close, whose own gate refused it for cause, and returns the patch it wrote,
// or nil when nothing was written. Keeping the close code would hold the row
// on the unknown-state skip forever, out of reach of every path that serves a
// dormant seat. The row becomes an ordinary asleep row instead, where a
// refused close leaves a seat whose runtime is gone (SleepPatch, keeping its
// sleep_reason, else runtime-missing, and clearing the close stamps). From the
// next tick its work wakes it, the stranded repair or a killed seat's claim
// release takes it, or the close's own closer runs again. The write is refused
// when the row changed since the tick's read; the next tick decides again.
func rollBackInterruptedClose(store beads.Store, info sessionpkg.Info, cause string, trace *sessionReconcilerTraceCycle, now time.Time, stderr io.Writer) sessionpkg.MetadataPatch {
	reason := strings.TrimSpace(info.SleepReason)
	if reason == "" {
		reason = string(sessionpkg.SleepReasonRuntimeMissing)
	}
	patch := sessionpkg.SleepPatch(now, reason)
	patch["close_reason"] = ""
	patch["closed_at"] = ""
	applied, err := sessionFrontDoor(store).ApplyPatchIfLifecycleUnchanged(info, patch)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "session reconciler: rolling back interrupted %q close of session bead %s: %v\n", info.MetadataState, info.ID, err) //nolint:errcheck
		recordInterruptedClose(trace, info, TraceOutcomeNoChange, traceRecordPayload{"cause": cause, "rollback_error": err.Error()})
		return nil
	case !applied:
		recordInterruptedClose(trace, info, TraceOutcomeNoChange, traceRecordPayload{"cause": cause, "rollback": "not_applied"})
		return nil
	}
	fmt.Fprintf(stderr, "WARN: reconciler: rolled back interrupted %q close of session bead %s to asleep — its close is refused (%s)\n", info.MetadataState, info.ID, cause) //nolint:errcheck
	recordInterruptedClose(trace, info, TraceOutcomeRollback, traceRecordPayload{"cause": cause, "sleep_reason": reason})
	return patch
}

// closeGateName names gate in trace payloads.
func closeGateName(gate closeGate) string {
	switch gate {
	case closeGated:
		return "gated"
	case closeFreeSlot:
		return "free_slot"
	case closeReleasing:
		return "releasing"
	case closeRollback:
		return "rollback"
	}
	return fmt.Sprintf("gate_%d", gate)
}

// recordInterruptedClose records the finisher's decision for info on trace.
func recordInterruptedClose(trace *sessionReconcilerTraceCycle, info sessionpkg.Info, outcome TraceOutcomeCode, payload traceRecordPayload) {
	if trace == nil {
		return
	}
	trace.RecordDecision(TraceSiteReconcilerFinishInterruptedClose, TraceReasonInterruptedClose, outcome, info.Template, info.SessionNameMetadata, payload)
}
