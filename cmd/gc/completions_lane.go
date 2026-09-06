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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
)

// Cadences are consts, not config. Every one of them is an operational knob
// somebody will eventually want per city, and every one of them is also a knob
// that lets an operator recreate the hot-backstop bug this lane exists to fix.
// TODO(ga-l7jdg): expose these under [daemon] once a city has needed a value
// other than the default; until then a const is one less way to be wrong.
const (
	// completionsBackstopInterval is the full sweep's cadence when nothing forces
	// it sooner.
	completionsBackstopInterval = time.Hour

	// completionsBackstopChunk caps the roots one background pass visits, so a
	// large closed-molecule corpus is converged over several passes instead of
	// one long one. The next pass resumes from the cursor rather than the start.
	completionsBackstopChunk = 64

	// completionsWarmFailureBackoff paces retries after a chunk that could not
	// warm its idempotency record. The chunk cadence exists to keep a HEALTHY
	// sweep moving; re-polling an unreadable journal at that cadence re-issues
	// the journal read every poll interval for as long as the journal stays
	// broken (ga-ftgyl). The debt is kept — only the retry is paced.
	completionsWarmFailureBackoff = 5 * time.Minute

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

	forced          bool
	sweepRan        bool
	lastSweepAt     time.Time
	lastSweepReason string
	// nextAttemptAt gates sweepDue after a warm failure. Zero means no backoff
	// is in force. It never clears the forced latch or the cadence: the sweep
	// stays owed, only the retry is paced.
	nextAttemptAt time.Time

	// Per-sweep accumulators. A sweep spans several chunks, so its summary line
	// has to be assembled across them or it reports one chunk and calls it a
	// convergence pass. The reason is latched from the chunk that STARTED the
	// sweep: a sweep that began because the feed declared a gap is a gap-driven
	// sweep even if its later chunks would have been due on cadence anyway.
	sweepStartedAt time.Time
	sweepReason    string
	sweepEmitted   int
	sweepRoots     int
	sweepSkipped   int

	interval time.Duration
	poll     time.Duration
}

func newCompletionsLane() *completionsLane {
	return &completionsLane{
		pending:  map[string]struct{}{},
		interval: completionsBackstopInterval,
		poll:     completionsBackstopChunkInterval,
		// Nothing has converged yet, so the first thing this lane does is sweep —
		// expressed by sweepRan being false rather than by pre-setting the forced
		// latch. Both make the first pass due; only this one lets it report itself
		// as a startup pass instead of as a cursor gap that never happened.
	}
}

// pollEvery is how often the background loop asks whether a chunk is due. It is
// a lane field rather than a bare const read so a test can drive the REAL loop
// and prove it re-arms past its startup sweep.
func (l *completionsLane) pollEvery() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.poll <= 0 {
		return completionsBackstopChunkInterval
	}
	return l.poll
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

// sweepDue reports whether the full convergence sweep should run now, and WHY.
//
// The reason is not decoration. A sweep running because the event feed declared
// a gap and a sweep running on its hourly cadence are different events with
// different follow-ups, and the trace field they both land in cannot tell them
// apart unless the lane says which. The reason also selects VisitStamped (only
// a startup sweep re-examines converged-stamped roots), so the ORDER of these
// cases is load-bearing.
//
// Startup is checked BEFORE forced, and must be: a startup sweep visits stamped
// AND unstamped roots, a strict superset of the cursor-gap sweep's unstamped
// walk, so answering "startup" for a boot that was also forced loses no
// coverage. The reverse order does lose: the delta-feed gap callback can
// force() the lane before its first sweep, and if forced won that race the boot
// sweep would be cursor-gap-labeled with VisitStamped=false — skipping stamped
// roots so the per-boot stale-stamp heal never runs on the one sweep that could.
// Once the first full traversal completes, noteSweepChunk sets sweepRan and
// clears forced together, so this case stops firing and a later force is
// correctly reported as a cursor gap.
func (l *completionsLane) sweepDue(now time.Time) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.nextAttemptAt) {
		return "", false
	}
	switch {
	case !l.sweepRan:
		return backstopReasonStartup, true
	case l.forced:
		return backstopReasonCursorGap, true
	case now.Sub(l.lastSweepAt) >= l.interval:
		return backstopReasonCadence, true
	default:
		return "", false
	}
}

// noteSweepFailure paces the lane after a chunk that could not warm its
// idempotency record. Nothing about the sweep's debt changes — forced stays
// latched, the cadence clock does not advance — the next attempt is simply
// deferred so an unreadable journal is not re-read every poll interval.
func (l *completionsLane) noteSweepFailure(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextAttemptAt = now.Add(completionsWarmFailureBackoff)
}

// noteSweepChunk folds one chunk into the sweep in progress and, when the chunk
// completed a full traversal, closes the sweep out: it clears the force latch,
// advances the cadence, and returns the whole sweep's totals for the summary
// line. A sweep still in progress returns done=false and keeps the lane due.
func (l *completionsLane) noteSweepChunk(now time.Time, reason string, emitted, roots, skipped int, complete bool) (total completionsSweepTotals, done bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sweepStartedAt.IsZero() {
		l.sweepStartedAt = now
		l.sweepReason = reason
	}
	l.sweepEmitted += emitted
	l.sweepRoots += roots
	l.sweepSkipped += skipped
	if !complete {
		return completionsSweepTotals{}, false
	}
	total = completionsSweepTotals{
		Emitted: l.sweepEmitted,
		Roots:   l.sweepRoots,
		Skipped: l.sweepSkipped,
		Elapsed: now.Sub(l.sweepStartedAt),
		Reason:  l.sweepReason,
	}
	l.lastSweepAt = now
	l.lastSweepReason = l.sweepReason
	l.sweepRan = true
	l.forced = false
	l.sweepStartedAt = time.Time{}
	l.sweepReason = ""
	l.sweepEmitted = 0
	l.sweepRoots = 0
	l.sweepSkipped = 0
	return total, true
}

// completionsSweepTotals is one full traversal's summary.
type completionsSweepTotals struct {
	Emitted int
	Roots   int
	// Skipped counts converged-stamped roots the sweep excluded from the
	// walk; visible so a sweep that visits 10 of 900 roots reads as the
	// optimization working, not as 890 roots vanishing.
	Skipped int
	Elapsed time.Duration
	// Reason is why the sweep was due, latched from its first chunk.
	Reason string
}

// lastSweep reports when the last FULL traversal finished, why it was due, and
// whether one ever has. It is what the tick's trace record carries so an
// operator can see the convergence lane's age and trigger without reading the
// log: a backstop whose age nobody can see is a backstop nobody notices has
// stopped, and one whose reason nobody can see is a gap indistinguishable from
// a schedule.
func (l *completionsLane) lastSweep() (at time.Time, reason string, ran bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastSweepAt, l.lastSweepReason, l.sweepRan
}

// completionsLaneOf returns this runtime's completions lane, creating it on
// first use so a directly-constructed runtime needs no wiring.
func (cr *CityRuntime) completionsLaneOf() *completionsLane {
	cr.completionsOnce.Do(func() { cr.completions = newCompletionsLane() })
	return cr.completions
}

// absorbCompletionFact hands one journal event to the delta pass's idempotency
// record, so a tick that names a root does not have to re-read the journal to
// learn what the journal already carries. A runtime with no controller state has
// no delta pass to keep warm.
func (cr *CityRuntime) absorbCompletionFact(evt events.Event) {
	if cr.cs == nil {
		return
	}
	cr.cs.completionsDeltaIndex.Absorb(evt)
}

// invalidateCompletionFacts drops the delta pass's idempotency record so the
// next pass rebuilds it from the journal. It shares the sweep's gap hook because
// it is the same gap: a feed that can no longer promise to name every event can
// no longer keep this record current either. The cost of being wrong here is a
// DUPLICATE recovery fact rather than a stranded repair, and one journal read
// after a gap is cheap insurance against it.
func (cr *CityRuntime) invalidateCompletionFacts() {
	if cr.cs == nil {
		return
	}
	cr.cs.completionsDeltaIndex.Invalidate()
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
	ticker := time.NewTicker(lane.pollEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		reason, due := lane.sweepDue(time.Now())
		if !due {
			continue
		}
		cr.safeTick(func() { cr.runCompletionsSweepChunk(backstop, lane, reason) }, "completions-sweep")
	}
}

// runCompletionsSweepChunk advances the convergence sweep by one chunk and
// reports what it repaired. The cadence latch advances only on a COMPLETE
// traversal, so a sweep spread over several chunks keeps running rather than
// being counted as done after its first one.
func (cr *CityRuntime) runCompletionsSweepChunk(backstop *executionevent.CompletionBackstop, lane *completionsLane, reason string) executionevent.CompletionBackstopResult {
	if cr.cs == nil {
		return executionevent.CompletionBackstopResult{SweepComplete: true}
	}
	ep, graphStores := cr.cs.completionReconcileInputs(reconcilePlane)
	if ep == nil {
		return executionevent.CompletionBackstopResult{}
	}
	// Startup sweeps re-examine stamped roots; the converged stamp is a
	// cadence optimization whose stale entries (hand-reopened steps, vacuous
	// stamps from a past wedge) are healed by the per-boot full traversal.
	backstop.VisitStamped = reason == backstopReasonStartup
	result := backstop.Pass(ep, graphStores, "execution-reconcile")
	if result.WarmFailed {
		// The chunk read nothing: its idempotency record could not be loaded.
		// Back the retry off instead of re-issuing the journal read at the
		// chunk cadence; the sweep debt (forced latch, cadence clock) is kept.
		lane.noteSweepFailure(time.Now())
		fmt.Fprintf(cr.stderr, "%s: completions sweep: journal warm failed; retrying in %s\n", cr.logPrefix, completionsWarmFailureBackoff) //nolint:errcheck // best-effort stderr
		return result
	}
	// A store the traversal could not list is skipped so one dark store cannot
	// stall the sweep. Skipped silently, that is a lane converging nothing while
	// looking healthy, so every skip is named.
	for _, listErr := range result.ListErrors {
		fmt.Fprintf(cr.stderr, "%s: completions sweep: %v\n", cr.logPrefix, listErr) //nolint:errcheck // best-effort stderr
	}
	total, done := lane.noteSweepChunk(time.Now(), reason, result.Emitted, result.RootsVisited, result.RootsSkippedConverged, result.SweepComplete)
	if done {
		summary := fmt.Sprintf("reason=%s converged %d root(s) (+%d already-converged skipped), emitted %d completion fact(s), took %s (stores=%d)",
			total.Reason, total.Roots, total.Skipped, total.Emitted, total.Elapsed.Round(time.Millisecond), len(graphStores))
		fmt.Fprintf(cr.stderr, "%s: completions sweep: %s\n", cr.logPrefix, summary) //nolint:errcheck // best-effort stderr
	}
	return result
}

// addBackstopAgeFields records a convergence lane's age on the tick record that
// consumes it.
//
// Both backstops run on background goroutines, so the tick is the only place
// their liveness is observable from the trace. "Never ran" is reported as a
// distinct value rather than as an age of zero: a lane that has not converged
// once and a lane that converged a moment ago are opposite conditions, and
// collapsing them is how a stalled backstop hides.
func addBackstopAgeFields(fields map[string]any, at time.Time, reason string, ran bool) {
	if !ran {
		fields["backstop_ran"] = false
		return
	}
	fields["backstop_ran"] = true
	fields["backstop_age_seconds"] = int(time.Since(at).Round(time.Second) / time.Second)
	if reason != "" {
		fields["backstop_last_reason"] = reason
	}
}
