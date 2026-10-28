package main

// The wisp GC lane: the expired-molecule purge runs on its own goroutine, off
// the tick (#2604).
//
// wisp_gc was a step of the serial tick, after the session reconcile. A purge
// over a large closed-molecule corpus can run for minutes, and every session
// decision that waited for the next tick (a restart a seat requested, a drain,
// a spawn) waited that long too. The purge decides nothing about sessions; it only
// deletes beads. It moves to a lane the way #5318 moved the orphan sweep and
// the completions read: same work, same cadence state (wispGC.shouldRun), no
// tick time.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	// wispGCLaneSafeTickTrigger names the lane in safeTick panic lines.
	wispGCLaneSafeTickTrigger = "wisp-gc-lane"
	// wispGCLaneTraceTrigger is the trace-cycle trigger for lane passes. The
	// wisp_gc operation record keeps its site and name; only the cycle it
	// lands in changes, from a controller tick to a wisp GC pass.
	wispGCLaneTraceTrigger TraceTickTrigger = "wisp_gc"
)

// startWispGCLane starts the lane and returns the function that stops it and
// waits for a pass in flight. The lane asks wispGC.shouldRun once per patrol
// interval. The tick also asked it on startup and event pokes, so a due purge
// can now start up to one patrol interval later, and a poke no longer runs one.
func (cr *CityRuntime) startWispGCLane(ctx context.Context) func() {
	interval := cr.serviceConfigSnapshot().Daemon.PatrolIntervalDuration()
	laneCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		pressureSkips := 0
		for {
			select {
			case <-laneCtx.Done():
				return
			case <-ticker.C:
			}
			cr.safeTick(func() { cr.runWispGCLanePass(&pressureSkips) }, wispGCLaneSafeTickTrigger)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// runWispGCLanePass runs the purge when it is due. It skips the gates the tick
// skipped it under: a quiescent city (no bead store is touched), and high FS
// pressure, where the tick forced a pass after maxConsecutiveFSPressureSkips.
func (cr *CityRuntime) runWispGCLanePass(pressureSkips *int) {
	if cr.beadsQuiescent.Load() {
		return
	}
	if status, ok := currentFSPressureStatus(cr.stderr); ok && status.High && *pressureSkips < maxConsecutiveFSPressureSkips {
		*pressureSkips++
		return
	}
	*pressureSkips = 0
	cfg, wg := cr.wispGCSnapshot()
	now := time.Now()
	if wg == nil || !wg.shouldRun(now) {
		return
	}
	cityStore := cr.cityBeadStore()
	graphStore := beads.GraphStore{Store: resolveGraphStore(cr.storageRoutes, cityStore, cfg, cr.cityPath, cr.rec)}
	if graphStore.Store == nil {
		return
	}
	sessionStore := resolveSessionStore(cr.storageRoutes, cityStore, cfg, cr.cityPath, cr.rec)
	sessionLedger := beads.SessionStore{Store: relocatedSQLiteSessionLedger(cr.storageRoutes, sessionStore, cityStore)}
	mailStore := beads.MailStore{Store: resolveMailMessagesStore(cr.storageRoutes, cityStore, cfg, cr.cityPath, cr.rec)}

	var trace *sessionReconcilerTraceCycle
	if cr.trace != nil {
		trace = cr.trace.beginCycle(sessionReconcilerTraceCycleInfo{
			TickTrigger: string(wispGCLaneTraceTrigger),
			CityPath:    cr.cityPath,
		}, cfg, nil)
	}
	completion := TraceCompletionAborted
	defer func() {
		if trace != nil {
			trace.end(completion, traceRecordPayload{"phase": "wisp_gc"})
		}
	}()

	phaseStart := time.Now()
	purged, gcErr := wg.runGC(graphStore, sessionLedger, mailStore, now)
	if trace != nil {
		trace.RecordControllerOperation(TraceSiteControllerTickPhase, TraceReasonRetained, TraceOutcomeComplete,
			"wisp_gc", time.Since(phaseStart), map[string]any{"purged": purged})
	}
	if gcErr != nil {
		for _, line := range strings.Split(gcErr.Error(), "\n") {
			if line == "" {
				continue
			}
			fmt.Fprintf(cr.stderr, "%s: wisp gc: %s\n", cr.logPrefix, line) //nolint:errcheck // best-effort stderr
		}
	}
	if purged > 0 {
		fmt.Fprintf(cr.stdout, "Bead GC: purged %d expired bead(s)\n", purged) //nolint:errcheck // best-effort stdout
	}
	completion = TraceCompletionCompleted
}

// wispGCSnapshot reads the published config and the wisp GC built from it,
// under the lock publishRuntimeConfig writes both with.
func (cr *CityRuntime) wispGCSnapshot() (*config.City, wispGC) {
	cr.serviceStateMu.RLock()
	defer cr.serviceStateMu.RUnlock()
	return cr.cfg, cr.wg
}
