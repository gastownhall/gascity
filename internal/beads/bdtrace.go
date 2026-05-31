package beads

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// reconcilerTickTrigger is the most recent reason the reconciler loop
// woke and ran cr.tick() — one of "patrol", "poke", or other trigger
// strings recognized by city_runtime.runTick. Updated atomically by
// SetReconcilerTickTrigger / ClearReconcilerTickTrigger and read by
// TraceBDCall to attribute bd calls to the tick reason that drove them.
//
// Best-effort. In a supervisor managing multiple concurrent cities,
// ticks from different cities can race and the value reflects the most
// recent setter. For diagnosis of one-city workloads it is accurate;
// for multi-city deployments treat the field as advisory and rely on
// the Callers field for definitive attribution.
var reconcilerTickTrigger atomic.Pointer[string]

// SetReconcilerTickTrigger records the current tick reason; returns the
// previous value so the caller can restore it on exit (supporting
// nested tick frames if they ever exist).
func SetReconcilerTickTrigger(trigger string) *string {
	return reconcilerTickTrigger.Swap(&trigger)
}

// RestoreReconcilerTickTrigger restores the previous tick-trigger
// pointer captured by SetReconcilerTickTrigger.
func RestoreReconcilerTickTrigger(prev *string) {
	reconcilerTickTrigger.Store(prev)
}

// classifyTraceScope inspects the Go call stack captured for a bd
// invocation and returns a high-level scope label so consumers can
// filter or aggregate without parsing function names themselves. The
// classifier walks the stack outermost-frame-first and returns the
// first scope whose marker is present; scopes are ordered from most
// specific (cobra-driven CLI commands, hook cascades) to most generic
// (reconciler tick).
//
// When no scope matches, returns "unknown" so analysis queries always
// have a non-empty field to group on.
func classifyTraceScope(callers []string) string {
	// Walk from innermost to outermost and return the first matching
	// scope. Innermost-first picks the MOST SPECIFIC scope when
	// multiple frames on the stack match (e.g. a call from
	// dispatchOrders inside tick() should classify as order-dispatch,
	// not tick-body). The catch-alls (tick-body, cli-command) sit at
	// the bottom of the switch so they only match when no specific
	// inner scope did.
	for _, fn := range callers {
		switch {
		// Hook cascade — bd write fired the on_<event> shell script,
		// which forked one of these gc subcommands. The cobra frame
		// is also present but the autoclose helper is more specific.
		case strings.Contains(fn, "main.doConvoyAutoclose"):
			return "hook:convoy-autoclose"
		case strings.Contains(fn, "main.doWispAutoclose"):
			return "hook:wisp-autoclose"

		// Event-bus driven work.
		case strings.Contains(fn, "applyBeadEventToStores"),
			strings.Contains(fn, "startBeadEventWatcher"):
			return "bead-event-watcher"

		// CachingStore self-maintenance.
		case strings.Contains(fn, "CachingStore).reconcileLoop"),
			strings.Contains(fn, "runReconciliation"),
			strings.Contains(fn, "PrimeActive"),
			strings.Contains(fn, "(*CachingStore).Prime"):
			return "cache-reconcile"

		// Startup / one-time init paths.
		case strings.Contains(fn, "initBeadsForDir"),
			strings.Contains(fn, "initAndHookDir"),
			strings.Contains(fn, "finalizeCanonicalBdScopeInit"),
			strings.Contains(fn, "verifyCanonicalBdScopeStoreReady"),
			strings.Contains(fn, "buildStores"),
			strings.Contains(fn, "(*controllerState).build"),
			strings.Contains(fn, "main.init.func"):
			return "init"

		// Order dispatch — includes the gating helpers that decide
		// whether to fire (hasOpenWork, LastRunFuncForStore, cursor
		// lookup, etc.). Without these the order-dispatch scope
		// shows up as "unknown" because the dispatch frame is
		// already deeper than the chokepoint sees.
		case strings.Contains(fn, "memoryOrderDispatcher).dispatch"),
			strings.Contains(fn, "memoryOrderDispatcher).hasOpenWork"),
			strings.Contains(fn, "memoryOrderDispatcher).cachedLastRun"),
			strings.Contains(fn, "dispatchOrders"),
			strings.Contains(fn, "orders.LastRunFuncForStore"),
			strings.Contains(fn, "orders.LastRunAcrossStores"),
			strings.Contains(fn, "orders.CursorAcrossStores"),
			strings.Contains(fn, "bdCursorAcrossStores"),
			strings.Contains(fn, "doOrderCheck"):
			return "order-dispatch"

		// Session observation hot path.
		case strings.Contains(fn, "workerObserveSessionTarget"),
			strings.Contains(fn, "loadProviderSessionSnapshot"):
			return "session-observe"

		// Per-session bead reconciliation work.
		case strings.Contains(fn, "reconcileSessionBeads"),
			strings.Contains(fn, "beadReconcileTick"):
			return "reconcile-session-beads"

		// Demand probing — work_query subprocesses + ready() per agent.
		case strings.Contains(fn, "loadDemandSnapshot"),
			strings.Contains(fn, "computeWorkSet"),
			strings.Contains(fn, "readyForControllerDemand"),
			strings.Contains(fn, "defaultScaleCheckCounts"),
			strings.Contains(fn, "defaultNamedSessionDemand"),
			strings.Contains(fn, "collectAssignedWorkBeadsWith"):
			return "demand-probe"

		// Session-bead bookkeeping (writes that mirror state into beads).
		case strings.Contains(fn, "syncSessionBeadsWith"),
			strings.Contains(fn, "syncBeadsAndUpdateIndex"),
			strings.Contains(fn, "(*Manager).confirmLiveSessionState"):
			return "sync-session-beads"

		// Session listing.
		case strings.Contains(fn, "loadSessionBeadSnapshot"),
			strings.Contains(fn, "listConfiguredNamedSessionBeadsByMetadata"),
			strings.Contains(fn, "listSessionBeadsByMetadata"),
			strings.Contains(fn, "ListAllSessionBeads"):
			return "session-list"

		// Session identity resolution — invoked by many entry points
		// (CLI commands, workerHandle, sling). Classify as resolve
		// only when no more-specific outer frame matched.
		case strings.Contains(fn, "ResolveSessionBeadByExactID"),
			strings.Contains(fn, "ResolveSessionIDByExactID"),
			strings.Contains(fn, "session.ResolveSessionID"),
			strings.Contains(fn, "session.(*Manager).loadSessionBead"),
			strings.Contains(fn, "resolveSessionIDWithOptions"),
			strings.Contains(fn, "resolveSessionIDMaterializingNamed"):
			return "session-resolve"

		// Mail subsystem — recipient route lookup is the hot one;
		// Read/Send are explicit ops.
		case strings.Contains(fn, "beadmail.(*Provider).filterMessages"),
			strings.Contains(fn, "beadmail.(*Provider).messageCandidatesForRoutes"),
			strings.Contains(fn, "beadmail.(*Provider).recipientRoutes"),
			strings.Contains(fn, "beadmail.(*Provider).recipientSessionMatches"),
			strings.Contains(fn, "resolveMailTargetsCached"):
			return "mail-routing"
		case strings.Contains(fn, "beadmail.(*Provider).Read"),
			strings.Contains(fn, "beadmail.(*Provider).Send"):
			return "mail-op"

		// Reaper / corpse cleanup paths.
		case strings.Contains(fn, "reapStaleSessionBeads"),
			strings.Contains(fn, "cleanupDeadRuntimeSessionCorpses"),
			strings.Contains(fn, "reapRuntimesBoundToClosedBeads"),
			strings.Contains(fn, "releaseOrphanedPoolAssignments"):
			return "session-reaper"

		// Session lifecycle: spawning, stopping, drain commits.
		case strings.Contains(fn, "prepareStartCandidateForCity"),
			strings.Contains(fn, "commitAsyncStartResultWithContext"),
			strings.Contains(fn, "executePlannedStarts"),
			strings.Contains(fn, "createPoolSessionBead"),
			strings.Contains(fn, "syncDesiredPoolSlots"):
			return "session-start"
		case strings.Contains(fn, "stopTargetThroughWorkerBoundary"),
			strings.Contains(fn, "workerStopSessionTarget"),
			strings.Contains(fn, "workerKillSessionTarget"):
			return "session-stop"
		case strings.Contains(fn, "withCitySessionIdentifierLock"):
			return "session-lock"
		case strings.Contains(fn, "(*CityRuntime).shutdown"):
			return "city-shutdown"

		// Order-tracking sweep (the periodic order body that closes
		// stale order-tracking beads, NOT the dispatcher itself).
		case strings.Contains(fn, "sweepStaleOrderTracking"),
			strings.Contains(fn, "sweepOrphanedOrderTracking"):
			return "order-tracking-sweep"

		// Explicit mail ops via CLI/Go that the broader classifier missed.
		case strings.Contains(fn, "doMailArchive"),
			strings.Contains(fn, "doMailRead"),
			strings.Contains(fn, "newMailSendCmd"),
			strings.Contains(fn, "newMailReplyCmd"),
			strings.Contains(fn, "newMailCheckCmd"),
			strings.Contains(fn, "cmdMail"):
			return "mail-op"

		// Sling — work routing.
		case strings.Contains(fn, "sling.CollectAttachedBeads"),
			strings.Contains(fn, "sling.dispatch"),
			strings.Contains(fn, "(*Sling)"):
			return "sling"

		// Generic tick-body catch-all for reconciler-driven calls
		// that didn't match a more specific scope above.
		case strings.Contains(fn, "(*CityRuntime).tick"):
			return "tick-body"

		// CLI-driven (gc bd, gc convoy, gc wisp, gc mail, etc.) —
		// catch-all for direct user invocations and hook subprocesses
		// not already classified above.
		case strings.Contains(fn, "cobra.(*Command).execute"):
			return "cli-command"
		}
	}
	return "unknown"
}

// bdTraceCallerSkip is how many frames to skip past TraceBDCall and
// the immediate site that invokes it (the BdStore runner closure or a
// bypass site like execPurge) to find the actual user-facing caller.
// Tuned per call site: the chokepoint runner is two frames deep
// (runner closure → method that called runner), so frame 3 is the
// originating BdStore method; frame 4 is the caller of that method.
// We capture both frames so analysis can roll up either way.
func captureBDTraceCallers() []string {
	// 24 frames is enough to reach the outermost runTick or cobra
	// entry point from the deepest BdStore call chains observed in
	// practice (~15 frames). Going much higher costs runtime walk
	// time on every traced call; lower truncates the outer scope
	// information classifyTraceScope relies on.
	var pcs [24]uintptr
	n := runtime.Callers(2, pcs[:])
	if n == 0 {
		return nil
	}
	frames := runtime.CallersFrames(pcs[:n])
	out := make([]string, 0, n)
	for {
		frame, more := frames.Next()
		if frame.Function != "" {
			out = append(out, frame.Function)
		}
		if !more || len(out) >= 16 {
			break
		}
	}
	return out
}

// TraceBDCall appends a JSONL record describing one bd subprocess
// invocation to the file pointed to by the GC_BD_TRACE_JSON env var. When
// GC_BD_TRACE_JSON is unset the call is a no-op.
//
// Designed to capture every bd subprocess spawned by gas city
// application code (BdStore chokepoint, doctor checks, gc bd
// passthrough, maintenance pack scripts, status-line scripts). Tag the
// source so consumers can filter by call site when analyzing.
//
// Best-effort: trace failures are silently ignored so a broken trace
// path can never break a real bd call.
//
// The trace is gated on the GC_BD_TRACE_JSON env var (NOT GC_BD_TRACE,
// which the existing line-format trace in bdstore.go uses). The two
// formats are incompatible, so separating the env vars lets operators
// enable one, the other, or both independently. When GC_BD_TRACE_JSON
// is unset, TraceBDCall returns immediately and is effectively a no-op.
func TraceBDCall(source, dir string, args []string, start time.Time, exitCode int, err error) {
	path := strings.TrimSpace(os.Getenv("GC_BD_TRACE_JSON"))
	if path == "" {
		return
	}
	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if openErr != nil {
		return
	}
	defer f.Close() //nolint:errcheck // best-effort trace log

	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	callers := captureBDTraceCallers()
	scope := classifyTraceScope(callers)
	tickTrigger := ""
	if p := reconcilerTickTrigger.Load(); p != nil {
		tickTrigger = *p
	}
	// Environment-provided scope hint: subprocesses spawned by gas
	// city (notably the bd-hook cascade and order exec bodies) inherit
	// GC_BD_TRACE_SCOPE so their bd calls record where they came from
	// even though they're a fresh process with no in-memory context.
	envScope := strings.TrimSpace(os.Getenv("GC_BD_TRACE_SCOPE"))

	rec := struct {
		TS          string   `json:"ts"`
		Source      string   `json:"source"`
		Scope       string   `json:"scope"`
		EnvScope    string   `json:"env_scope,omitempty"`
		TickTrigger string   `json:"tick_trigger,omitempty"`
		Args        []string `json:"args"`
		Dir         string   `json:"dir,omitempty"`
		DurMs       int64    `json:"dur_ms"`
		ExitCode    int      `json:"exit_code"`
		Err         string   `json:"err,omitempty"`
		PID         int      `json:"pid"`
		PPID        int      `json:"ppid"`
		Callers     []string `json:"callers,omitempty"`
	}{
		TS:          time.Now().UTC().Format(time.RFC3339Nano),
		Source:      source,
		Scope:       scope,
		EnvScope:    envScope,
		TickTrigger: tickTrigger,
		Args:        args,
		Dir:         dir,
		DurMs:       time.Since(start).Milliseconds(),
		ExitCode:    exitCode,
		Err:         errMsg,
		PID:         os.Getpid(),
		PPID:        os.Getppid(),
		Callers:     callers,
	}
	data, marshalErr := json.Marshal(rec)
	if marshalErr != nil {
		return
	}
	data = append(data, '\n')
	_, _ = f.Write(data) //nolint:errcheck // best-effort trace log
}
