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
}

// laneWorkerHooks are what a host supplies to a conversation's worker.
type laneWorkerHooks struct {
	// pass runs one full pass over the conversation.
	pass func() lanePassOutcome
	// yield reports whether the worker gives up its slot rather than hold
	// it through a wait of d.
	yield func(d time.Duration) bool
	// release gives the slot up and reports true; with force false it keeps
	// the slot and reports false when a poke arrived since the last pass.
	release func(force bool) bool
	// park pokes the conversation again after d without a slot; st is the
	// state its next worker starts from.
	park func(d time.Duration, st laneWorkerState)
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
				if !w.hooks.release(false) {
					break // a poke arrived since the pass: pass again
				}
				if !idle {
					w.hooks.park(delay, w.state)
				}
				return
			}
			end := w.wait(delay)
			if end == laneWaitEnded {
				w.hooks.release(true)
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
	// Take moved the conversation: pass again at once to re-plan.
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
	// to start the session. A held row, or one that is not dormant (a crash
	// not yet healed, a failed create), changes on no schedule of ours.
	switch out.wake {
	case session.WakeRecorded:
		take(laneWakeRecheck)
	case session.WakeHeld, session.WakeNotDormant:
		take(laneWakeHeldRecheck)
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

// laneProbeStillBusy reports whether a re-probe found the runtime as a busy
// pass left it: running and live, nothing blocking content, no pending
// interaction, and no boundary yet. Anything else is a change a full pass
// must plan.
func laneProbeStillBusy(p opjournal.Probe) bool {
	return p.Available && p.Running && p.Caps.Live && p.ContentBlock == "" && p.Pending == "" &&
		p.Boundary != opjournal.TriYes
}
