package main

// The interrupted-close recognizer and finisher (gastownhall/gascity#2085).
//
// A legacy close is two writes on a non-atomic store: the terminal metadata
// patch (session.ClosePatch: state carries the close code) and the status
// flip to closed (Store.CommitClose). When the second write is lost — the
// process died between them, or the store's Tx is not atomic across the two
// bd calls — the row is left half-closed: open, with its own close code as
// the metadata state. That state is not in knownSessionStates, so the
// reconciler's unknown-state skip holds the row open forever: an open bead
// owns its session_name, a max=1 pool cannot route around its own canonical
// identity, and the template is stuck at zero seats until an operator closes
// the bead by hand.
//
// The recognizer maps the row back to the close it was dying from, and the
// finisher retakes that close under the same discipline the failed-create
// close already uses: never while observation is partial, never while the
// runtime may still be alive, and always through closeSeat's gates.

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

// interruptedCloseCodeGates maps the fixed close codes — terminal state values
// this binary's own close paths stamp via session.ClosePatch — to the close
// gate each path's full close takes (the closeWhy literals at their call
// sites). The gates match those call sites exactly: duplicate and
// reconfigured closes gate on the seat holding nothing; the sweep's
// gc_swept close frees the slot; the corpse, stale-reap and stranded-repair
// closes release the seat's work instead of gating on it.
var interruptedCloseCodeGates = map[string]closeGate{
	"duplicate":               closeGated,
	"reconfigured":            closeGated,
	"gc_swept":                closeFreeSlot,
	"stale-session":           closeReleasing,
	"dead-runtime":            closeReleasing,
	strandedRepairCloseReason: closeReleasing,
}

// interruptedClosePassthroughCodes is the other half of the recognizer's
// vocabulary: close codes a close passes through from the row's sleep_reason
// (the passthrough close stamps the trimmed sleep_reason as its terminal
// state). It is the freeable-sleep admit list isPoolSessionSlotFreeableInfo
// reads, minus the states the reconciler already knows (drained,
// failed-create).
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

// interruptedCloseGate recognizes a half-closed session row and returns the
// gate its close takes, or 0 when the row's state is not this binary's own
// interrupted close. It keys off the RAW metadata state, matching
// isKnownStateInfo: a padded variant (" duplicate") is a genuinely unknown
// state, not a close code. A passthrough code counts only in the exact shape
// its close writes it: a pool-managed, non-named row whose sleep_reason
// carries the same code.
func interruptedCloseGate(info sessionpkg.Info) closeGate {
	if gate, ok := interruptedCloseCodeGates[info.MetadataState]; ok {
		return gate
	}
	if !interruptedClosePassthroughCodes[info.MetadataState] {
		return 0
	}
	if strings.TrimSpace(info.SleepReason) != info.MetadataState {
		return 0
	}
	if !isPoolManagedSessionInfo(info) || isNamedSessionInfo(info) {
		return 0
	}
	return closeGated
}

// interruptedCloseGateName is the trace payload's name for a close gate.
func interruptedCloseGateName(gate closeGate) string {
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
	return "unknown"
}

// finishInterruptedClose finishes the interrupted close of a half-closed
// session row: the row's metadata state is this binary's own close code, but
// its status flip to closed was lost. Mirroring the failed-create close's
// discipline, it skips while the tick's observation is partial, fails closed
// when liveness observation errors, keeps the row open while its runtime is
// alive, and otherwise releases the row-owned runtime lease and takes the
// close through closeSeat under the recognizer's gate — so the close cascade
// (releasing work, freeing the identity) runs exactly as the original close's
// would have. It reports whether the row closed; a refusal leaves the row to
// the unknown-state signal (S20) exactly as the skip did.
func finishInterruptedClose(
	cityPath string,
	store beads.Store,
	sw *SeatWork,
	sp runtime.Provider,
	cfg *config.City,
	dt *drainTracker,
	info sessionpkg.Info,
	gate closeGate,
	storeQueryPartial bool,
	deferSessionClosesOnBoot bool,
	trace *sessionReconcilerTraceCycle,
	clk clock.Clock,
	stderr io.Writer,
) bool {
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = info.Template
	}
	if storeQueryPartial || deferSessionClosesOnBoot {
		if trace != nil {
			trace.RecordDecision(TraceSiteReconcilerFinishInterruptedClose, TraceReasonInterruptedClose, TraceOutcomeSkipped, template, info.SessionNameMetadata, traceRecordPayload{
				"store_query_partial":          storeQueryPartial,
				"defer_session_closes_on_boot": deferSessionClosesOnBoot,
			})
		}
		return false
	}
	providerAlive, livenessErr := workerSessionTargetRunningWithConfig(cityPath, store, sp, cfg, info.ID)
	if livenessErr != nil {
		// Fail closed, as the failed-create close does: providerAlive=false
		// with an observation error is "unavailable", not "confirmed dead".
		// The level-triggered loop re-observes next tick.
		fmt.Fprintf(stderr, "session reconciler: skipping interrupted close of '%s': liveness observation failed: %v\n", info.SessionNameMetadata, livenessErr) //nolint:errcheck
		if trace != nil {
			trace.RecordDecision(TraceSiteReconcilerFinishInterruptedClose, TraceReasonInterruptedClose, TraceOutcomeSkippedLivenessError, template, info.SessionNameMetadata, traceRecordPayload{
				"liveness_error": livenessErr.Error(),
			})
		}
		return false
	}
	if providerAlive {
		// The close decided on a dead runtime, but the runtime is alive now
		// (a restart raced the close, or the close read a transient exit).
		// The row is not a corpse; keep it open for the ordinary lifecycle.
		if trace != nil {
			trace.RecordDecision(TraceSiteReconcilerFinishInterruptedClose, TraceReasonInterruptedClose, TraceOutcomeKeptOpen, template, info.SessionNameMetadata, traceRecordPayload{
				"provider_alive": true,
			})
		}
		return false
	}
	closed := releaseRowOwnedRuntimeLeased(cityPath, store, info, sp, stderr, standingRowRuntimeHold(dt, stderr, info.ID, clk.Now())) &&
		closeSeat(store, sw, info, closeWhy{reason: info.MetadataState, gate: gate}, clk.Now().UTC(), stderr)
	if closed {
		// A stranded repair or a passthrough (sleep-reason) close prunes the
		// agent-home worktree its full close prunes.
		if info.MetadataState == strandedRepairCloseReason || interruptedClosePassthroughCodes[info.MetadataState] {
			pruneAgentHomeWorktreeIfSafeInfo(info, cityPath, cfg, stderr)
		}
		fmt.Fprintf(stderr, "WARN: reconciler: finished interrupted %q close of session bead %s — runtime not running\n", info.MetadataState, info.ID) //nolint:errcheck
		if trace != nil {
			trace.RecordDecision(TraceSiteReconcilerFinishInterruptedClose, TraceReasonInterruptedClose, TraceOutcomeClosed, template, info.SessionNameMetadata, traceRecordPayload{
				"state": info.MetadataState,
				"gate":  interruptedCloseGateName(gate),
			})
		}
		return true
	}
	if trace != nil {
		// Same refusal class as the failed-create close: a gate held the
		// close (the seat still holds open assigned work), so "closed" would
		// be a lie for as long as the seat stays wedged.
		trace.RecordDecision(TraceSiteReconcilerFinishInterruptedClose, TraceReasonInterruptedClose, TraceOutcomeNoChange, template, info.SessionNameMetadata, nil)
	}
	return false
}
