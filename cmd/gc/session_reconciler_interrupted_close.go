package main

import (
	"fmt"
	"io"
	"strings"

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
// a known state, and the close that wrote it is gated on the row's prior
// state, so nothing would ever run it again: the row would hold its pool or
// named identity forever. The reconciler finishes such a close before the
// unknown-state skip.

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
var interruptedClosePassthroughCodes = map[string]bool{
	"idle-timeout": true,
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
// the same ClosePatch and runs the same guards and cascade. Any refusal leaves
// the row to the unknown-state skip. Each refusal is recorded on trace at
// TraceSiteReconcilerFinishInterruptedClose.
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
) bool {
	if storeQueryPartial || deferOnBoot {
		return false
	}
	alive, err := workerSessionTargetRunningWithConfig(cityPath, store, sp, cfg, info.ID)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: skipping interrupted close of '%s': liveness observation failed: %v\n", info.SessionNameMetadata, err) //nolint:errcheck
		recordInterruptedClose(trace, info, TraceOutcomeSkippedLivenessError, traceRecordPayload{
			"liveness_error": err.Error(),
		})
		return false
	}
	if alive {
		recordInterruptedClose(trace, info, TraceOutcomeKeptOpen, nil)
		return false
	}
	if !releaseRowOwnedRuntimeLeased(cityPath, store, info, sp, stderr, standingRowRuntimeHold(dt, stderr, info.ID, clk.Now())) {
		recordInterruptedClose(trace, info, TraceOutcomeKeptOpen, nil)
		return false
	}
	if !closeSeat(store, sw, info, closeWhy{reason: info.MetadataState, gate: gate}, clk.Now().UTC(), stderr) {
		return false
	}
	if info.MetadataState == strandedRepairCloseReason || interruptedClosePassthroughCodes[info.MetadataState] {
		pruneAgentHomeWorktreeIfSafeInfo(info, cityPath, cfg, stderr)
	}
	return true
}

// recordInterruptedClose records the finisher's decision for info on trace.
func recordInterruptedClose(trace *sessionReconcilerTraceCycle, info sessionpkg.Info, outcome TraceOutcomeCode, payload traceRecordPayload) {
	if trace == nil {
		return
	}
	trace.RecordDecision(TraceSiteReconcilerFinishInterruptedClose, TraceReasonInterruptedClose, outcome, info.Template, info.SessionNameMetadata, payload)
}
