package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// The v2 planner loop (architecture §1.3, §1.5): one goroutine that schedules
// passes and solely owns the reconciler's mutable state, so none of that
// state takes a lock. Effects report back through the settlement queue; every
// other input reaches the planner as a dirty mark.
//
// Unwired: C2c1 supplies the pass (tracePass), and C2c2 constructs the
// planner behind session_reconciler=v2.

// plannerMinGap is the shortest time from one pass's start to the next's.
const plannerMinGap = 250 * time.Millisecond

// plannerPaceFactor spaces pass starts at this many times the pace duration
// (paceGap), which keeps the duty cycle near 1/plannerPaceFactor.
const plannerPaceFactor = 4

// passFunc runs one pass at now. C2c1 supplies gather, allocate, decideRow
// and admit.
type passFunc func(now time.Time) passResult

// passResult is what a pass reports back to the loop.
type passResult struct {
	Next   time.Time // earliest row deadline; zero for none
	Counts passCounts
}

// passRecord is the last pass, for the status surfaces OBS1 adds.
type passRecord struct {
	Start    time.Time
	Duration time.Duration
	Panicked bool
	Result   passResult
}

// settlement is an effect's report that it finished. Seq echoes the
// in-flight entry's submit. A create's names its entry by Token, since the
// create has no row until it lands. A zero At is stamped with the drain time.
type settlement struct {
	Key     rowKey
	Kind    string
	Reason  string // the intent's reason, which the pass record counts by
	Seq     uint64
	Token   string
	Outcome settleOutcome
	// Cause is a refusal's or failure's cause. The backoff table keeps it
	// verbatim: admit reads causeFinalizePrefix (P4), and the allocation
	// createStageFence (F3).
	Cause string
	// BackoffKey is the record a refusal or failure backs off and a landing
	// or no-op resets, under Fingerprint: the row's when empty; a create's
	// createBackoffKey under its ConfigRev, set only when it landed or was
	// refused at a stage.
	BackoffKey, Fingerprint string
	Facts                   effectFacts // applied once drained (applyFacts)
	Err                     error
	At                      time.Time
	// Late marks what an effect returned after the executor settled it: the
	// drain applies only its facts.
	Late bool
	// Closed marks an effect whose close section's close landed: what
	// around reads to run a post-close cascade once, never for an earlier
	// write or a row another writer closed.
	Closed bool
}

// drainTransition is one RecordDrainTransition.
type drainTransition struct{ Name, Reason, Transition string }

// settleOutcome is how an effect ended (CONTRACT v5 S1, P5).
type settleOutcome uint8

const (
	settledLanded    settleOutcome = iota + 1
	settledFailed                  // ran and failed; a panic or deadline included
	settledRefused                 // refused before or instead of writing, with a cause
	settledAmbiguous               // its write began and may have landed, or a create was abandoned
	settledNoop                    // nothing to do: a start's already-running
	settleOutcomeEnd               // one past the last: the simulator's census gates each outcome
)

// plannerInflight is the in-flight map the planner owns: *inflightMap
// (reconcile_inflight.go), or a test's fake.
type plannerInflight interface {
	add(inflightEntry) uint64
	settle(settlement)
	clearVisible(inflightCensus, time.Time) []clearRecord
	view() inflightView
}

// bootState is the boot gate (CONTRACT v5 P2): no destructive intent until
// the cache is primed, every backend of the latest inventory pass is primed
// and, when a demand leg is lane-fed, the first external-reads recording
// exists. gather computes it each pass; admit applies it.
type bootState struct {
	CachePrimed, InventoryComplete, RecordingSeen bool
}

func (b bootState) open() bool { return b.CachePrimed && b.InventoryComplete && b.RecordingSeen }

// planner runs passes on one goroutine. Only that goroutine touches inflight,
// backoff, bucket, fairSeed, boot, last, rowTrace, idleProbes, memo and
// stalled; other goroutines reach the planner through markDirty, the
// settlement queue, the execution-stalled inbox, the start pause and stop,
// and read what a pass publishes through out.
type planner struct {
	clock       plannerClock
	seam        txSeamFunc           // every pass's (effectPass.seam): stagingSeam's, or a test's
	patrol      func() time.Duration // read after every pass, so a reload takes effect
	pass        passFunc
	stopEffects func(deadline time.Time) // the executor's stop
	effects     *effectExecutor          // submits what admission lets through; nil while trace-only
	creates     *createEffects           // runs create effects on effects; nil defers creates (no-effect)
	rec         events.Recorder          // settlements' events; nil records none
	stranded    *strandedClaimAlerts     // the controller's session.stranded_claims alerter
	// classified is each row's last classified dead runtime, by its token
	// (A12, the start's recycle; v5 S5, X10): in memory, so a restart
	// classifies once more.
	classified map[rowKey]classifiedRuntime
	stderr     io.Writer
	metrics    *passMetrics
	emitRecord func(fields map[string]any) // the reconcile.pass record; nil emits none

	// observations is the cache a fresh read's Noted fact is written to; nil
	// notes none. capacity is the endpoint breaker a start admits its ticket
	// on; nil admits without one.
	observations func() *ObservationCache
	capacity     func() *endpointCapacityGuard

	inflight plannerInflight
	backoff  *backoffTable
	bucket   bucketState
	fairSeed uint64 // admission's fair-share rotation (P4)
	boot     bootState
	last     passRecord
	rowTrace map[rowKey]string // each row's last traced (reason, outcome)
	// aliasOwed are the rows whose alias write landed with their runtime
	// not yet synced (A7's runtime-alias sync): set by the write's fact,
	// cleared when the sync settles.
	aliasOwed map[rowKey]bool
	// claimsSeen is when the planner first saw each open work assignment
	// (claimsSeenAt), for the scale_check pairing guard.
	claimsSeen map[workClaim]time.Time
	// idle is A15's memory (idleState), lost on restart as legacy's
	// trackers are.
	idle idleMemory
	// idleProbes are the idle begins submitted to prove an agent idle, by
	// row, which paceIdleProbes holds a patrol apart.
	idleProbes map[rowKey]idleProbe
	memo       gatherMemo
	out        passOutputs
	obs        passObserver

	// stalled holds the execution-stalled requests by row ID for arm A16
	// (C7b1); C8's step posts them to stalledPosted under stalledMu. A
	// request is one-shot: it goes once its drain begin lands, so an
	// operator who resumes the row is not drained again from it. A
	// re-trigger is the backstop's new request, which its latch holds to
	// one per instance token per 30 minutes (stalledLatchHolds), as legacy's.
	softReload    atomic.Pointer[softReloadRequest] // the open soft reload (M2)
	stalled       map[string]executionStalledRequest
	stalledMu     sync.Mutex
	stalledPosted []executionStalledRequest

	dirty       chan struct{} // capacity 1: marks fold until the loop reads one
	settlements settlementQueue
	paused      atomic.Bool
	quit        chan struct{}
	quitOnce    sync.Once
	running     atomic.Bool
	done        chan struct{} // closed when run returns
}

func newPlanner(clock plannerClock, patrol func() time.Duration, pass passFunc, inflight plannerInflight, stopEffects func(time.Time), stderr io.Writer) *planner {
	p := &planner{
		clock: clock, patrol: patrol, pass: pass, stopEffects: stopEffects, stderr: stderr,
		metrics:   newPassMetrics(),
		inflight:  inflight,
		backoff:   newBackoffTable(),
		rowTrace:  make(map[rowKey]string),
		aliasOwed: make(map[rowKey]bool),
		dirty:     make(chan struct{}, 1),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	p.settlements.wake = func() { p.markDirty("settlement") }
	return p
}

// markDirty asks for a pass. It never blocks: marks made while one is
// pending, or while a pass runs, fold into one follow-up pass.
func (p *planner) markDirty(reason string) {
	p.metrics.recordWake(reason)
	select {
	case p.dirty <- struct{}{}:
	default:
	}
}

// postExecutionStalled hands the planner an execution-stalled request
// (NUDGE-022) from another goroutine, and asks for a pass.
func (p *planner) postExecutionStalled(r executionStalledRequest) {
	p.stalledMu.Lock()
	p.stalledPosted = append(p.stalledPosted, r)
	p.stalledMu.Unlock()
	p.markDirty("execution-stalled")
}

// executionStalled folds the posted requests into the planner's, newest per
// row, forgets the spent ones, and returns the pass's copy. A request is
// spent (C7b1 one-shot, mc-uinl0 ruling) when a complete c holds its row
// closed; when the pass reads its row, at the request's generation, with the
// execution-stalled stop request on it (its begin landed, whatever its
// settlement said), dormant, or with its runtime gone or a corpse (obs at
// now within maxAge, on a pass started after the post), which is where
// legacy's tracked drain completes; or 30
// minutes after it was posted, the backstop latch's re-post cadence
// (executionStalledLatchRetryAfter). A stall that persists is posted again.
func (p *planner) executionStalled(c *sessionCensus, obs *ObservationSnapshot, now time.Time, maxAge time.Duration) map[string]executionStalledRequest {
	p.stalledMu.Lock()
	posted := p.stalledPosted
	p.stalledPosted = nil
	p.stalledMu.Unlock()
	if p.stalled == nil {
		p.stalled = make(map[string]executionStalledRequest)
	}
	for _, r := range posted {
		p.stalled[r.ID] = r
	}
	if len(p.stalled) == 0 {
		return nil
	}
	rows := make(map[string]censusRow, len(p.stalled))
	for _, row := range c.Canonical() {
		if _, ok := p.stalled[row.Info.ID]; ok {
			rows[row.Info.ID] = row
		}
	}
	complete, ok := c.complete()
	out := make(map[string]executionStalledRequest, len(p.stalled))
	for id, r := range p.stalled {
		row, found := rows[id]
		switch {
		case ok && complete.ClosedID(id),
			!r.At.IsZero() && now.Sub(r.At) >= executionStalledLatchRetryAfter,
			found && stallRequestSpentOn(r, row, obs, now, maxAge):
			delete(p.stalled, id)
			continue
		}
		out[id] = r
	}
	return out
}

// stallRequestSpentOn reports whether row, read by this pass at r's
// generation, has spent r: the execution-stalled stop request is on it, it
// is dormant, or its runtime is gone or a corpse.
func stallRequestSpentOn(r executionStalledRequest, row censusRow, obs *ObservationSnapshot, now time.Time, maxAge time.Duration) bool {
	gen, _ := strconv.ParseInt(strings.TrimSpace(r.Generation), 10, 64) // an empty generation reads 0, as A16's
	if gen != row.Incarnation {
		return false
	}
	if req, active := activeStop(row); active && req.Reason == executionStalledDrainReason { // at the row's generation, gen
		return true
	}
	if !sessionBeadClaimsLiveRuntime(row.Info) {
		return true
	}
	// Absence or a corpse spends it only on a pass that started after the
	// post: an older listing may predate the runtime the backstop judged.
	if obs == nil || !obs.Inventory.StartedAt.After(r.At) {
		return false
	}
	switch readRuntimeName(obs, row.Info.SessionName, now, maxAge).state {
	case nameAbsent, nameCorpse:
		return true
	}
	return false
}

// aliasOwedFor forgets the owed runtime-alias syncs of rows a complete c
// holds closed, and returns the pass's copy.
func (p *planner) aliasOwedFor(c *sessionCensus) map[rowKey]bool {
	if len(p.aliasOwed) == 0 {
		return nil
	}
	complete, ok := c.complete()
	for k := range p.aliasOwed {
		if ok && complete.Closed(k) {
			delete(p.aliasOwed, k)
		}
	}
	return maps.Clone(p.aliasOwed)
}

// pauseStarts and resumeStarts bracket a provider swap; the pass reads
// startsPaused and admits no start while it is set.
func (p *planner) pauseStarts()       { p.paused.Store(true) }
func (p *planner) resumeStarts()      { p.paused.Store(false) }
func (p *planner) startsPaused() bool { return p.paused.Load() }

// run schedules passes until ctx ends or stop is called. A pass is owed after
// a dirty mark, the patrol interval after the last pass ends (bounding a lost
// wake, R-6), or the last pass's earliest row deadline. One timer serves the
// patrol, the row deadline and the pacing wait. An owed pass starts at once
// if the planner is idle; otherwise it waits paceGap after the last start.
func (p *planner) run(ctx context.Context) {
	p.running.Store(true)
	defer close(p.done)
	timer := p.clock.NewTimer(p.patrol())
	defer timer.Stop()
	due, dueReason := p.clock.Now().Add(p.patrol()), "patrol"
	owed := false
	for {
		if ctx.Err() != nil || p.stopping() {
			return
		}
		now := p.clock.Now()
		wakeAt := due
		if owed {
			wakeAt = p.last.Start.Add(p.paceGap())
			if !now.Before(wakeAt) {
				owed = false
				res := p.runPass(now)
				due, dueReason = p.clock.Now().Add(p.patrol()), "patrol"
				if !res.Next.IsZero() && res.Next.Before(due) {
					due, dueReason = res.Next, "deadline"
				}
				continue
			}
		}
		timer.Reset(wakeAt.Sub(now))
		select {
		case <-ctx.Done():
			return
		case <-p.quit:
			return
		case <-p.dirty:
			owed = true
		case <-timer.C():
			if !owed {
				p.metrics.recordWake(dueReason)
				owed = true
			}
		}
	}
}

// paceGap is the wait from the last pass's start to an owed pass's
// (CONTRACT v5.9j P1): plannerPaceFactor times the last pass's duration,
// at least plannerMinGap, and at most the patrol's due time, one patrol
// after the last pass ended. So the idle time after a pass is at least
// min(3 × its duration, the patrol): passes run at most a quarter of the
// time, or d/(d+patrol) for a pass of d longer than a third of the patrol,
// and an owed pass is never later than the patrol would have run it. A 101s
// freeze once bought a 404s stall (staging J40 C); now at most a patrol.
func (p *planner) paceGap() time.Duration {
	return max(plannerMinGap, min(plannerPaceFactor*p.last.Duration, p.last.Duration+p.patrol()))
}

// runPass applies the settlements posted since the last pass
// (drainSettlements), then runs the pass. A panic in either is recovered in
// safeTick's style: the pass is skipped and logged, a follow-up pass is
// owed, and the first of a run of panicking passes alerts. Then the pass is
// observed (observePass).
func (p *planner) runPass(now time.Time) (res passResult) {
	defer func() {
		r := recover()
		if r != nil {
			fmt.Fprintf(p.stderr, "v2 planner: pass panicked: %v (type=%T)\n%s\n", r, r, debug.Stack()) //nolint:errcheck // best-effort stderr
			p.markDirty("panic")
			if !p.last.Panicked {
				p.alert(alertPassPanic, "", fmt.Sprintf("pass panicked: %v", r))
			}
		}
		p.last = passRecord{Start: now, Duration: p.clock.Now().Sub(now), Panicked: r != nil, Result: res}
		p.metrics.recordPass(now, p.last.Duration, r != nil, res.Counts)
		p.observePass(now)
	}()
	p.drainSettlements(now)
	return p.pass(now)
}

// drainSettlements applies the settlements posted so far, stamping a zero
// At with now: first every one to the in-flight map, so no later step's
// panic can leave a settled effect counted in flight; then to the backoff
// table and the pass record's counters; then their facts (applyFacts). A
// late settlement applies only its facts.
func (p *planner) drainSettlements(now time.Time) {
	items := p.settlements.drain()
	for i := range items {
		if items[i].At.IsZero() {
			items[i].At = now
		}
		if !items[i].Late {
			p.inflight.settle(items[i])
		}
	}
	for _, s := range items {
		if !s.Late {
			p.backoffSettled(s)
			p.observeSettlement(s)
			p.softReload.Load().settle(s)
			if s.Kind == intentRuntimeAlias && (s.Outcome == settledLanded || s.Outcome == settledNoop) {
				delete(p.aliasOwed, s.Key)
			}
		}
		if s.Kind == intentDrainBegin && s.Reason == decideDrainBegin+executionStalledDrainReason && s.Outcome == settledLanded {
			delete(p.stalled, s.Key.ID) // its drain begin landed: the request is spent
		}
	}
	for _, s := range items {
		p.applyFacts(s.Facts, s.At)
	}
}

// applyFacts applies an effect's facts, on time or late alike: its events to
// the recorder, each recovered alone; its drain transition and its
// quarantine to legacy's telemetry; its worktree verdict to the work item's
// backoff record (C6.5(a)); an alias write's owed runtime sync to the
// planner's set. Every field is consumed here (TestApplyFactsConsumesEveryField).
func (p *planner) applyFacts(f effectFacts, at time.Time) {
	for _, ev := range f.Events {
		if p.rec != nil {
			p.record(ev)
		}
	}
	if t := f.Transition; t != nil {
		recordDrainTransition(context.Background(), t.Name, t.Reason, t.Transition)
	}
	if f.Quarantined != "" {
		recordAgentQuarantine(context.Background(), f.Quarantined)
	}
	if f.IdleKilled != "" {
		recordAgentIdleKill(context.Background(), f.IdleKilled)
	}
	if f.AliasOwed != nil {
		p.aliasOwed[*f.AliasOwed] = true
	}
	if s := f.Stopped; s != nil {
		recordAgentStop(context.Background(), s.Name, s.Identity, "drain-ack", nil)
	}
	switch {
	case f.Work == nil:
	case f.Work.Refused:
		p.backoff.Refuse(workBackoffKey(f.Work.BeadID), at, time.Time{}, createStageWorktree, f.Work.Fingerprint)
	default:
		p.backoff.Succeed(workBackoffKey(f.Work.BeadID))
	}
	if c := f.Classified; c != nil {
		if p.classified == nil {
			p.classified = make(map[rowKey]classifiedRuntime)
		}
		p.classified[c.Key] = *c
	}
	if n := f.Noted; n != nil && p.observations != nil {
		if cache := p.observations(); cache != nil && (!n.Clears || goneNoted(cache, n.Name)) {
			// Listed, running and the agent alive (or none of them, for a
			// runtime gone), at the read's issue time, so a later inventory
			// pass overrides it either way (v5 O4). An effect's alive read
			// is written only over a name the cache reads unlisted (noteFresh).
			v := ObsYes
			if n.Gone {
				v = ObsNo
			}
			for _, kind := range [...]FactKind{FactListed, FactRunning, FactProcessAlive} {
				cache.Note(n.Name, kind, v, n.At, SourceProbe, "")
			}
		}
	}
}

// goneNoted reports that the cache reads name unlisted: an effect's gone
// note, or an inventory's absence, which a newer fresh alive read overrides
// with nothing of the pass's to lose.
func goneNoted(cache *ObservationCache, name string) bool {
	return cache.Snapshot().ByName[name].Listed.Value == ObsNo
}

// recordDrainTransition, recordAgentQuarantine, recordAgentIdleKill and
// recordAgentStop are legacy's drain, quarantine, idle-kill and stop
// telemetry; a test reads them.
var (
	recordDrainTransition = telemetry.RecordDrainTransition
	recordAgentQuarantine = telemetry.RecordAgentQuarantine
	recordAgentIdleKill   = telemetry.RecordAgentIdleKill
	recordAgentStop       = telemetry.RecordAgentStop
	recordAgentCrash      = telemetry.RecordAgentCrash // the recycle's and A12's crash metric
)

// record records ev, counting the stop-outstanding alerts (v5 D6) for the
// pass record; a panicking recorder is logged and skips this event only.
func (p *planner) record(ev events.Event) {
	if ev.Type == events.SessionDrainStopEscalated {
		p.metrics.count(&p.metrics.series, "stop_escalations")
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(p.stderr, "v2 planner: recording %s panicked: %v\n", ev.Type, r) //nolint:errcheck // best-effort stderr
		}
	}()
	p.rec.Record(ev)
}

// backoffSettled applies s to the backoff table (P4): a landing or no-op
// resets its record; a refusal (a landing with a cause, a heal written in a
// refusal's stead, included), failure or ambiguous outcome (an abandoned
// write) backs it off with its cause, except a swap pause and an endpoint-gate
// refusal, deferrals the gate itself paces. A drift's landing, its restart
// or its bounded deferral's stamp (whose bound paces what follows), and a
// row's plain start each end the drift's episode: its record resets.
func (p *planner) backoffSettled(s settlement) {
	key := s.BackoffKey
	if key == "" && s.Key.ID != "" {
		key = settledBackoffKey(s.Kind, s.Reason, s.Key)
	}
	drift := s.Key.ID != "" && key == driftBackoffKey(s.Key)
	if s.Kind == intentStart && s.Outcome == settledLanded && s.Cause == "" && s.Key.ID != "" {
		p.backoff.Succeed(driftBackoffKey(s.Key))
	}
	switch {
	case key == "":
	case s.Outcome == settledLanded && (s.Cause == "" || drift) || s.Outcome == settledNoop:
		p.backoff.Succeed(key)
	case (s.Outcome == settledRefused || s.Outcome == settledLanded || s.Outcome == settledFailed || s.Outcome == settledAmbiguous) && s.Cause != causeSwapPause && s.Cause != causeEndpointGate && s.Cause != causeCapacity:
		p.backoff.Refuse(key, s.At, time.Time{}, s.Cause, s.Fingerprint)
	}
}

func (p *planner) stopping() bool {
	select {
	case <-p.quit:
		return true
	default:
		return false
	}
}

// stop shuts the planner down (GUAR-013): it stops admission first, so no
// pass starts after this call, then stops the executor until deadline, then
// waits for run to return. A pass already running finishes; the stopped
// executor refuses what it submits. Later calls only wait for run.
func (p *planner) stop(deadline time.Time) {
	p.quitOnce.Do(func() {
		close(p.quit)
		if p.stopEffects != nil {
			p.stopEffects(deadline)
		}
	})
	if p.running.Load() {
		<-p.done
	}
}

// settlementQueue carries settlements from effect goroutines to the planner.
// It is unbounded (D-8), so post never blocks and an effect never has to
// reason about the planner's progress to report back.
type settlementQueue struct {
	mu    sync.Mutex
	items []settlement
	wake  func() // a capacity-1 notify: the planner's dirty mark
}

func (q *settlementQueue) post(s settlement) {
	q.mu.Lock()
	q.items = append(q.items, s)
	q.mu.Unlock()
	q.wake()
}

func (q *settlementQueue) drain() []settlement {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

// plannerClock is the planner's clock, faked in tests. WithDeadline is
// context.WithDeadline on this clock: the executor hands effects a real
// deadline, which start-path code keys on.
type plannerClock interface {
	Now() time.Time
	NewTimer(d time.Duration) plannerTimer
	WithDeadline(parent context.Context, t time.Time) (context.Context, context.CancelFunc)
}

// plannerTimer is a one-shot timer. Reset discards a pending fire, as
// time.Timer does from Go 1.23.
type plannerTimer interface {
	C() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

type realPlannerClock struct{}

func (realPlannerClock) Now() time.Time { return time.Now() }

func (realPlannerClock) WithDeadline(parent context.Context, t time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(parent, t)
}

func (realPlannerClock) NewTimer(d time.Duration) plannerTimer {
	return realPlannerTimer{time.NewTimer(d)}
}

type realPlannerTimer struct{ t *time.Timer }

func (r realPlannerTimer) C() <-chan time.Time   { return r.t.C }
func (r realPlannerTimer) Reset(d time.Duration) { r.t.Reset(d) }
func (r realPlannerTimer) Stop()                 { r.t.Stop() }

// classifiedOf is the planner's classified memory for the census's rows,
// forgetting a row that left the census or whose runtime the pass reads
// alive or gone (SESS-090: a runtime that recovers and dies again is
// classified again).
func (p *planner) classifiedOf(c *sessionCensus, observed map[rowKey]rowObservation) map[rowKey]classifiedRuntime {
	for k := range p.classified {
		_, ok := c.Rows[k]
		if l := observed[k].Liveness; !ok || l == livenessAlive || l == livenessGone {
			delete(p.classified, k)
		}
	}
	return maps.Clone(p.classified)
}
