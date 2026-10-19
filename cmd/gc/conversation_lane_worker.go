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
}

// conversationLaneWorker serves one conversation until nothing it can see
// will change by itself, or its host ends.
type conversationLaneWorker struct {
	ctx      context.Context
	clk      clock.Clock
	newTimer func(time.Duration) (<-chan time.Time, func())
	poke     <-chan struct{}
	hooks    laneWorkerHooks
	state    laneWorkerState
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
)

// run passes over the conversation until it is idle or yields its slot, then
// releases the slot. Between passes it waits; a wait that ends on its timer
// after a busy pass re-probes, and only a change runs the next full pass.
func (w *conversationLaneWorker) run() {
	for {
		out := w.hooks.pass()
		if w.hooks.onPass != nil {
			w.hooks.onPass(out)
		}
		for {
			delay, idle := w.nextDelay(out)
			if idle || w.hooks.yield(delay) {
				var park *lanePark
				if !idle {
					park = &lanePark{after: delay, state: w.state}
				}
				if !w.hooks.release(false, park) {
					break // a poke arrived since the pass: pass again
				}
				return
			}
			end := w.wait(delay)
			if end == laneWaitEnded {
				w.hooks.release(true, nil)
				return
			}
			if end == laneWaitPoked || delay <= 0 || !w.reprobeDue(out) {
				break
			}
			still := out.reprobe()
			if w.hooks.onReprobe != nil {
				w.hooks.onReprobe(still)
			}
			if !still {
				break
			}
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

// wait sleeps for d, or until a poke or the host's end.
func (w *conversationLaneWorker) wait(d time.Duration) laneWaitEnd {
	if d <= 0 {
		if w.ctx.Err() != nil {
			return laneWaitEnded
		}
		return laneWaitFired
	}
	fire, stop := w.newTimer(d)
	defer stop()
	select {
	case <-w.ctx.Done():
		return laneWaitEnded
	case <-w.poke:
		return laneWaitPoked
	case <-fire:
		return laneWaitFired
	}
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
// probe admits some content past, as a draining row admits the drain
// notice a busy pass left waiting), no pending interaction, and no boundary
// yet. Anything else is a change a full pass must plan.
func laneProbeStillBusy(p opjournal.Probe) bool {
	return p.Available && p.Running && p.Caps.Live && (p.ContentBlock == "" || len(p.ContentAdmit) > 0) &&
		p.Pending == "" && p.Boundary != opjournal.TriYes
}
