package main

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/events"
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

// plannerPaceFactor spaces pass starts at this many times the last pass's
// duration, which keeps the duty cycle near 1/plannerPaceFactor.
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
	// Work is a create's worktree verdict, which backs off or resets the
	// work item's record (C6.5(a)).
	Work *workVerdict
	// Event is recorded once the settlement is drained: a landed write's.
	Event *events.Event
	Err   error
	At    time.Time
}

// settleOutcome is how an effect ended (CONTRACT v5 S1, P5).
type settleOutcome uint8

const (
	settledLanded    settleOutcome = iota + 1
	settledFailed                  // ran and failed; a panic or deadline included
	settledRefused                 // refused before or instead of writing, with a cause
	settledAmbiguous               // a create whose write call errored after the row may have landed
	settledNoop                    // nothing to do: a start's already-running
)

// plannerInflight is the in-flight map the planner owns: *inflightMap
// (reconcile_inflight.go), or a test's fake.
type plannerInflight interface {
	settle(settlement)
	view() inflightView
}

// bootState is the boot gate (CONTRACT v5 P2): no destructive intent until
// the cache is primed, every backend of the latest inventory pass is primed
// and, when a demand leg is lane-fed, the first external-reads recording
// exists. gather computes it each pass; admit applies it.
type bootState struct {
	CachePrimed, InventoryComplete, RecordingSeen bool
}

// planner runs passes on one goroutine. Only that goroutine touches inflight,
// backoff, bucket, fairSeed, boot, last, rowTrace, memo and stalled; other
// goroutines reach the planner through markDirty, the settlement queue, the
// execution-stalled inbox, the start pause and stop, and read what a pass
// publishes through out.
type planner struct {
	clock       plannerClock
	patrol      func() time.Duration // read after every pass, so a reload takes effect
	pass        passFunc
	stopEffects func(deadline time.Time) // the executor's stop
	rec         events.Recorder          // settlements' events; nil records none
	stderr      io.Writer
	metrics     *passMetrics

	inflight plannerInflight
	backoff  *backoffTable
	bucket   bucketState
	fairSeed uint64 // admission's fair-share rotation (P4)
	boot     bootState
	last     passRecord
	rowTrace map[rowKey]string // each row's last traced (reason, outcome)
	memo     gatherMemo
	out      passOutputs

	// stalled holds the execution-stalled requests by row ID for arm A16
	// (C7b1); C8's step posts them to stalledPosted under stalledMu.
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
		metrics:  newPassMetrics(),
		inflight: inflight,
		backoff:  newBackoffTable(),
		rowTrace: make(map[rowKey]string),
		dirty:    make(chan struct{}, 1),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
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
// row, forgets rows c no longer holds, and returns the pass's copy.
func (p *planner) executionStalled(c *sessionCensus) map[string]executionStalledRequest {
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
	open := make(map[string]bool)
	for _, row := range c.Canonical() {
		open[row.Key.ID] = true
	}
	out := make(map[string]executionStalledRequest, len(p.stalled))
	for id, r := range p.stalled {
		if !open[id] {
			delete(p.stalled, id)
			continue
		}
		out[id] = r
	}
	return out
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
// if the planner is idle; otherwise it waits until max(plannerMinGap,
// plannerPaceFactor × last duration) after the last start.
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
			wakeAt = p.last.Start.Add(max(plannerMinGap, plannerPaceFactor*p.last.Duration))
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

// runPass applies the settlements posted since the last pass
// (drainSettlements), then runs the pass. A panic in either is recovered in
// safeTick's style: the pass is skipped and logged, and a follow-up pass is
// owed.
func (p *planner) runPass(now time.Time) (res passResult) {
	defer func() {
		r := recover()
		if r != nil {
			fmt.Fprintf(p.stderr, "v2 planner: pass panicked: %v (type=%T)\n%s\n", r, r, debug.Stack()) //nolint:errcheck // best-effort stderr
			p.markDirty("panic")
		}
		p.last = passRecord{Start: now, Duration: p.clock.Now().Sub(now), Panicked: r != nil, Result: res}
		p.metrics.recordPass(now, p.last.Duration, r != nil, res.Counts)
	}()
	p.drainSettlements(now)
	return p.pass(now)
}

// drainSettlements applies the settlements posted so far, stamping a zero
// At with now: first every one to the in-flight map, so no later step's
// panic can leave a settled effect counted in flight; then to the backoff
// table; then their events to the recorder, each recovered alone.
func (p *planner) drainSettlements(now time.Time) {
	items := p.settlements.drain()
	for i := range items {
		if items[i].At.IsZero() {
			items[i].At = now
		}
		p.inflight.settle(items[i])
	}
	for _, s := range items {
		p.backoffSettled(s)
	}
	for _, s := range items {
		if s.Event != nil && p.rec != nil {
			p.record(*s.Event)
		}
	}
}

// record records ev; a panicking recorder is logged and skips this event
// only.
func (p *planner) record(ev events.Event) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(p.stderr, "v2 planner: recording %s panicked: %v\n", ev.Type, r) //nolint:errcheck // best-effort stderr
		}
	}()
	p.rec.Record(ev)
}

// backoffSettled applies s to the backoff table (P4): a landing or no-op
// resets its record; a refusal or failure backs it off with its cause,
// except a swap pause. A create's worktree verdict backs off or resets the
// work item's record.
func (p *planner) backoffSettled(s settlement) {
	key := s.BackoffKey
	if key == "" && s.Key.ID != "" {
		key = rowBackoffKey(s.Key)
	}
	switch {
	case key == "":
	case s.Outcome == settledLanded || s.Outcome == settledNoop:
		p.backoff.Succeed(key)
	case (s.Outcome == settledRefused || s.Outcome == settledFailed) && s.Cause != causeSwapPause:
		p.backoff.Refuse(key, s.At, time.Time{}, s.Cause, s.Fingerprint)
	}
	switch {
	case s.Work == nil:
	case s.Work.Refused:
		p.backoff.Refuse(workBackoffKey(s.Work.BeadID), s.At, time.Time{}, createStageWorktree, s.Work.Fingerprint)
	default:
		p.backoff.Succeed(workBackoffKey(s.Work.BeadID))
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
