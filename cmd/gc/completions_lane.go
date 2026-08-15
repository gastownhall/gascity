package main

// The completions lane: the same events-for-freshness / scan-for-convergence
// split the route-recovery lane runs, applied to graph.v2 completion facts.
//
// # What was wrong
//
// A controller can crash between a durable graph-step close and the best-effort
// `execution.step_completed` journal append, and graph stores intentionally emit
// no bead.closed at all — so a missed close would be a permanent lifecycle gap
// with nothing to repair it. ReconcileCompletedStores is that repair, and it is
// a convergence backstop: it re-derives the whole truth from current state.
//
// It was gated on `trigger == "patrol"`, which is not a cadence. Under overload
// every ticker fire that survives coalescing IS a patrol trigger, so the gate
// degraded to "every tick", and the pass walked every workflow root ever created
// — closed ones included, a corpus that only grows — on each one. 72.4s +/- 0.9s
// of a ~360s tick, constant, which is exactly the signature of a fixed corpus
// (ga-l7jdg, measured on ga-4qdfn).
//
// # The split
//
//   - The tick runs the DELTA pass: the roots the journal named since the last
//     pass, and nothing else. Roots are named by an execution.step_* fact's RunID
//     and by a bead.closed step snapshot's gc.root_bead_id. A steady tick names
//     none and reads neither the stores nor the journal.
//   - The full pass becomes a background sweep, chunked and resumable so a corpus
//     bigger than one chunk cannot starve its own convergence, plus the startup
//     pass that was already there.
//
// Trigger-name gating is gone on purpose. Explicit cadence state is the in-tree
// shape (wisp_gc.shouldRun, orderRescanInterval); "patrol" meaning "always" under
// load is precisely how this backstop ended up hot.

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
)

const (
	// completionsBackstopInterval is the full sweep's cadence when nothing forces
	// it sooner.
	completionsBackstopInterval = time.Hour

	// completionsBackstopChunk caps the roots one background pass visits, so a
	// large closed-molecule corpus is converged over several passes instead of
	// one long one. The next pass resumes from the cursor rather than the start.
	completionsBackstopChunk = 64

	// completionsBackstopChunkInterval paces the chunks of one sweep. A sweep of
	// a corpus larger than a chunk therefore takes several minutes of wall clock
	// and no tick time at all, which is the trade this lane exists to make.
	completionsBackstopChunkInterval = 30 * time.Second

	// completionsCandidateCap bounds the pending root set. Overflow is a gap:
	// the feed can no longer claim to name every changed root, so the sweep
	// answers instead of candidates being dropped.
	completionsCandidateCap = 4096
)

// completionsLane holds the delta feed's pending roots and the sweep's cadence.
type completionsLane struct {
	mu sync.Mutex

	pending map[string]struct{}

	forced      bool
	sweepRan    bool
	lastSweepAt time.Time

	interval time.Duration
}

func newCompletionsLane() *completionsLane {
	return &completionsLane{
		pending:  map[string]struct{}{},
		interval: completionsBackstopInterval,
		// Nothing has converged yet, so the first thing this lane does is sweep.
		forced: true,
	}
}

func (l *completionsLane) force() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.forced = true
}

// observe feeds one journal event to the lane, keeping only the graph.v2 root it
// names. An event that names no root costs the tick nothing.
func (l *completionsLane) observe(evt events.Event) {
	rootID := completionRootFromEvent(evt)
	if rootID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) >= completionsCandidateCap {
		l.pending = map[string]struct{}{}
		l.forced = true
		return
	}
	l.pending[rootID] = struct{}{}
}

// completionRootFromEvent extracts the execution run a journal event names.
//
// Two shapes carry one: an execution.step_* fact states its RunID outright, and
// a bead.closed notification carries the physical step snapshot whose
// gc.root_bead_id is the root. Between them they cover every way a step's
// closure becomes visible to this process.
func completionRootFromEvent(evt events.Event) string {
	switch evt.Type {
	case events.ExecutionStepCompleted, events.ExecutionStepStarted, events.ExecutionStepDefined:
		return strings.TrimSpace(evt.RunID)
	case events.BeadClosed:
		step, ok := beads.DecodeBeadEventPayload(evt.Payload)
		if !ok {
			return ""
		}
		return strings.TrimSpace(step.Metadata[beadmeta.RootBeadIDMetadataKey])
	default:
		return ""
	}
}

// takePending drains the named-root set.
func (l *completionsLane) takePending() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) == 0 {
		return nil
	}
	out := make([]string, 0, len(l.pending))
	for id := range l.pending {
		out = append(out, id)
	}
	l.pending = map[string]struct{}{}
	return out
}

// sweepDue reports whether the full convergence sweep should run now.
func (l *completionsLane) sweepDue(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.forced || !l.sweepRan {
		return true
	}
	return now.Sub(l.lastSweepAt) >= l.interval
}

// noteSweepRan records a completed sweep and clears the force latch. A sweep in
// progress (chunked, not yet complete) does NOT call this: its remaining chunks
// keep the lane due.
func (l *completionsLane) noteSweepRan(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSweepAt = now
	l.sweepRan = true
	l.forced = false
}

// completionsLaneOf returns this runtime's completions lane, creating it on
// first use so a directly-constructed runtime needs no wiring.
func (cr *CityRuntime) completionsLaneOf() *completionsLane {
	cr.completionsOnce.Do(func() { cr.completions = newCompletionsLane() })
	return cr.completions
}

// runCompletionsSweepLoop drives the whole-corpus convergence sweep off-tick,
// one bounded chunk at a time.
//
// Chunked so a large closed-molecule corpus converges in bounded steps that hold
// no store handle for minutes, and RESUMABLE so a corpus larger than one chunk
// cannot starve its own convergence by re-walking the same prefix forever. The
// sweep's cadence latch only advances when a full traversal completes, so a
// half-finished sweep keeps running rather than waiting out the hour.
func (cr *CityRuntime) runCompletionsSweepLoop(ctx context.Context, lane *completionsLane) {
	if cr.cs == nil {
		return
	}
	backstop := &executionevent.CompletionBackstop{ChunkSize: completionsBackstopChunk}
	ticker := time.NewTicker(completionsBackstopChunkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !lane.sweepDue(time.Now()) {
			continue
		}
		cr.safeTick(func() { cr.runCompletionsSweepChunk(backstop, lane) }, "completions-sweep")
	}
}

// runCompletionsSweepChunk advances the convergence sweep by one chunk and
// reports what it repaired. The cadence latch advances only on a COMPLETE
// traversal, so a sweep spread over several chunks keeps running rather than
// being counted as done after its first one.
func (cr *CityRuntime) runCompletionsSweepChunk(backstop *executionevent.CompletionBackstop, lane *completionsLane) executionevent.CompletionBackstopResult {
	if cr.cs == nil {
		return executionevent.CompletionBackstopResult{SweepComplete: true}
	}
	ep, graphStores := cr.cs.completionReconcileInputs(reconcilePlane)
	if ep == nil {
		return executionevent.CompletionBackstopResult{}
	}
	result := backstop.Pass(ep, graphStores, "execution-reconcile")
	if result.SweepComplete {
		lane.noteSweepRan(time.Now())
	}
	return result
}
