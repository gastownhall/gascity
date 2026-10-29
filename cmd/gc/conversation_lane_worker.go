package main

// One conversation's lane worker (DES-BX §10): the pass loop, the wait and
// backoff after each pass, the wake recheck, and the busy re-probe that costs
// no store read. It knows nothing of slots: the host that runs it, the
// controller's lane host today and a single-conversation host later (BX-3),
// supplies them through laneWorkerHooks.

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// lanePassOutcome is one worker pass.
type lanePassOutcome struct {
	res     opjournal.PassResult
	err     error
	skipped bool
	wake    session.WakeRequestOutcome
	// wakeErr is why the wake request the pass owed failed.
	wakeErr error
	// reprobe, set after a busy pass whose executor can re-probe, looks at
	// the runtime again without reading any row and reports whether it is
	// still busy. Nil means the next look is a full pass.
	reprobe func() bool
}

// laneWorkerState is what a worker carries from pass to pass, and across a
// park.
type laneWorkerState struct {
	busySince time.Time
	errors    int
	// wokeAt is when a pass last recorded a wake. A take, a pass that
	// waits on anything but the woken session's start, or startFor passing
	// clears it.
	wokeAt time.Time
	// turns says the worker parked with observations pending, so the next
	// worker watches the conversation's turns again whatever its first pass
	// finds.
	turns bool
}

// lanePark is how a worker that yields its slot parks its conversation: the
// conversation is poked again after after, and its next worker starts from
// state.
type lanePark struct {
	after time.Duration
	state laneWorkerState
}

// laneWorkerHooks are what a host supplies to a conversation's worker.
type laneWorkerHooks struct {
	// pass runs one full pass over the conversation.
	pass func() lanePassOutcome
	// yield reports whether the worker gives up its slot rather than hold
	// it through a wait of d.
	yield func(d time.Duration) bool
	// release gives the slot up and reports true; with force false it keeps
	// the slot and reports false when a poke arrived since the last pass. A
	// non-nil park parks the conversation in the same step, so no doorbell
	// finds it neither served nor parked.
	release func(force bool, park *lanePark) bool
	// onPass and onReprobe observe each full pass and each re-probe.
	onPass    func(lanePassOutcome)
	onReprobe func(stillBusy bool)
	// turns is the conversation's turn updates while its observer watches
	// (TI-3), nil otherwise; the worker hands each to onTurn while it waits,
	// and every queued one before it releases its slot, with ok false when
	// the channel closed.
	turns  func() <-chan worker.TurnUpdate
	onTurn func(u worker.TurnUpdate, ok bool)
	// turnsPending reports whether the observer has observations pending,
	// and how long the worker waits for them before it looks again; while
	// it does, the worker serves the conversation even with no op work
	// (DES-O1 §7.1). onTurnTimer runs when that wait ends on its timer.
	turnsPending func() (time.Duration, bool)
	onTurnTimer  func()
}

// conversationLaneWorker serves one conversation until nothing it can see
// will change by itself, or its host ends.
type conversationLaneWorker struct {
	ctx      context.Context
	clk      clock.Clock
	newTimer func(time.Duration) (<-chan time.Time, func())
	poke     <-chan struct{}
	// queued wakes a wait for the observer alone when a conversation
	// queued for a slot, so the worker weighs its slot again (hooks.yield).
	queued <-chan struct{}
	hooks  laneWorkerHooks
	state  laneWorkerState
	// startFor is how long a woken session may take to start: the city's
	// [session] startup_timeout.
	startFor time.Duration
}

// laneWaitEnd is how a worker's wait ended.
type laneWaitEnd int

const (
	laneWaitFired laneWaitEnd = iota
	laneWaitPoked
	laneWaitEnded
	// laneWaitSettled: a turn update changed what the observer waits for:
	// nothing is pending any more, or it wants to look sooner.
	laneWaitSettled
	// laneWaitQueued: a conversation queued for a slot while the worker
	// waited for its observer alone.
	laneWaitQueued
)

// run passes over the conversation until it is idle, with no observation
// pending, or yields its slot, then releases the slot.
func (w *conversationLaneWorker) run() {
	for {
		out := w.hooks.pass()
		if w.hooks.onPass != nil {
			w.hooks.onPass(out)
		}
		if !w.await(out) {
			return
		}
	}
}

// await waits after the pass that gave out until the next full pass is due,
// reporting true, or until the worker released its slot, reporting false.
// Between passes it waits for the earlier of the ops' next change and the
// observer's next look; a wait that ends on the ops' timer after a busy pass
// re-probes, and only a change runs the next full pass. A worker with no op
// work stays while observations are pending.
func (w *conversationLaneWorker) await(out lanePassOutcome) bool {
	delay, idle := w.nextDelay(out)
	due := w.clk.Now().Add(delay)
	for {
		hold, observing := w.turnsPending()
		if idle && !observing {
			w.drainTurns()
			hold, observing = w.turnsPending()
		}
		d, forTurns := delay, false
		if observing && (idle || hold < delay) {
			d, forTurns = hold, true
		}
		if (idle && !observing) || w.hooks.yield(d) {
			var park *lanePark
			if !idle || observing {
				state := w.state
				state.turns = observing
				park = &lanePark{after: d, state: state}
			}
			// A poke that arrived since the pass keeps the slot: pass again.
			return !w.hooks.release(false, park)
		}
		switch w.wait(d, idle && forTurns, forTurns) {
		case laneWaitEnded:
			w.hooks.release(true, nil)
			return false
		case laneWaitPoked:
			return true
		case laneWaitFired:
			if forTurns && w.hooks.onTurnTimer != nil {
				w.hooks.onTurnTimer()
			}
			if forTurns && (idle || w.clk.Now().Before(due)) {
				break
			}
			if delay <= 0 || !w.reprobeDue(out) {
				return true
			}
			still := out.reprobe()
			if w.hooks.onReprobe != nil {
				w.hooks.onReprobe(still)
			}
			if !still {
				return true
			}
			delay, idle = w.nextDelay(out)
			due = w.clk.Now().Add(delay)
			continue
		}
		if !idle {
			delay = max(due.Sub(w.clk.Now()), 0)
		}
	}
}

// turnsPending is the observer's answer, false without one.
func (w *conversationLaneWorker) turnsPending() (time.Duration, bool) {
	if w.hooks.turnsPending == nil {
		return 0, false
	}
	return w.hooks.turnsPending()
}

// drainTurns hands every turn update already queued to the observer, so a
// worker that releases its slot leaves none unfolded.
func (w *conversationLaneWorker) drainTurns() {
	for {
		select {
		case u, ok := <-w.turnUpdates():
			w.hooks.onTurn(u, ok)
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// reprobeDue reports whether the wait just ended re-probes instead of
// passing: the pass was busy, its executor can re-probe, and no waiting op
// is due to change by itself.
func (w *conversationLaneWorker) reprobeDue(out lanePassOutcome) bool {
	if out.reprobe == nil {
		return false
	}
	next := out.res.Next
	return next.IsZero() || w.clk.Now().Before(next)
}

// wait sleeps for d, or until a poke or the host's end, or, with yieldable
// set, until a conversation queues for a slot. A turn update that arrives
// meanwhile is handed to the observer and the wait goes on, on the same
// timer, unless the observer now wants to look sooner (a retry), or, with
// settle set, no observation is pending any more: the wait then ends
// settled, for the worker to plan again.
func (w *conversationLaneWorker) wait(d time.Duration, settle, yieldable bool) laneWaitEnd {
	if d <= 0 {
		if w.ctx.Err() != nil {
			return laneWaitEnded
		}
		return laneWaitFired
	}
	deadline := w.clk.Now().Add(d)
	fire, stop := w.newTimer(d)
	defer stop()
	var queued <-chan struct{}
	if yieldable {
		queued = w.queued
	}
	for {
		select {
		case <-w.ctx.Done():
			return laneWaitEnded
		case <-w.poke:
			return laneWaitPoked
		case <-queued:
			return laneWaitQueued
		case <-fire:
			return laneWaitFired
		case u, ok := <-w.turnUpdates():
			w.hooks.onTurn(u, ok)
			hold, pending := w.turnsPending()
			if (settle && !pending) || (pending && hold < deadline.Sub(w.clk.Now())) {
				return laneWaitSettled
			}
		}
	}
}

// turnUpdates is the observer's channel, nil (never ready) without one.
func (w *conversationLaneWorker) turnUpdates() <-chan worker.TurnUpdate {
	if w.hooks.turns == nil || w.hooks.onTurn == nil {
		return nil
	}
	return w.hooks.turns()
}

// nextDelay is how long the worker waits after out, or idle when nothing it
// can see will change by itself.
func (w *conversationLaneWorker) nextDelay(out lanePassOutcome) (time.Duration, bool) {
	ws := &w.state
	now := w.clk.Now()
	switch {
	case out.skipped:
		return laneQuiescentRetry, false
	case out.err != nil, out.wakeErr != nil:
		ws.errors++
		d := laneErrBackoffStart << min(ws.errors-1, 10)
		return min(d, laneErrBackoffCap), false
	}
	ws.errors = 0
	res := out.res
	// A pass that took something planned its waits and Next before the
	// Take moved the conversation: pass again at once to re-plan. A take
	// also ends the busy stretch, so the next turn's backoff starts over.
	if res.Took != opjournal.TakeNone {
		ws.busySince, ws.wokeAt = time.Time{}, time.Time{}
	}
	if res.More || res.Took != opjournal.TakeNone {
		return 0, false
	}
	var delay time.Duration
	have := false
	take := func(d time.Duration) {
		if !have || d < delay {
			delay, have = max(d, 0), true
		}
	}
	if res.Busy || laneWaitsFor(res, opjournal.WaitUnavailable) {
		if ws.busySince.IsZero() {
			ws.busySince = now
		}
		switch since := now.Sub(ws.busySince); {
		case since < laneBusyFirstFor:
			take(laneBusyFirst)
		case since < laneBusyNextFor:
			take(laneBusyNext)
		default:
			take(laneBusyMax)
		}
	} else {
		ws.busySince = time.Time{}
	}
	// Only a recorded wake is worth a quick look: the reconciler is about
	// to start the session, and a look that finds it still starting asks
	// for no wake, so the looks stay quick until startFor after the wake.
	// A held row, or one that is not dormant (a crash not yet healed, a
	// failed create), changes on no schedule of ours.
	switch {
	case out.wake == session.WakeRecorded:
		ws.wokeAt = now
		take(laneWakeRecheck)
	case out.wake == session.WakeHeld, out.wake == session.WakeNotDormant:
		ws.wokeAt = time.Time{}
		take(laneWakeHeldRecheck)
	case !ws.wokeAt.IsZero() && now.Sub(ws.wokeAt) < w.startFor && laneWaitsForStart(res):
		take(laneWakeRecheck)
	default:
		ws.wokeAt = time.Time{}
	}
	if !res.Next.IsZero() {
		take(res.Next.Sub(now))
	}
	return delay, !have
}

// laneWaitsFor reports whether any op of res waits for reason r.
func laneWaitsFor(res opjournal.PassResult, r opjournal.WaitReason) bool {
	for _, w := range res.Waits {
		if w.Reason == r {
			return true
		}
	}
	return false
}

// laneWaitsForStart reports whether an op of res waits for its session to
// finish starting (a content_block wait for session.ContentGateStarting).
func laneWaitsForStart(res opjournal.PassResult) bool {
	for _, w := range res.Waits {
		if w.Reason == opjournal.WaitContentBlock && w.Detail == session.ContentGateStarting {
			return true
		}
	}
	return false
}

// laneProbeStillBusy reports whether a re-probe found the runtime as a busy
// pass left it: running and live, nothing blocking content (or a block the
// probe admits some content past, as a draining or stop-requested row
// admits the drain notice a busy pass left waiting), no pending interaction, and no boundary
// yet. Anything else is a change a full pass must plan.
func laneProbeStillBusy(p opjournal.Probe) bool {
	return p.Available && p.Running && p.Caps.Live && (p.ContentBlock == "" || len(p.ContentAdmit) > 0) &&
		p.Pending == "" && p.Boundary != opjournal.TriYes
}
