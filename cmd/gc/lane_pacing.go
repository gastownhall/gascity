package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// lanePassSlowThreshold is how long a background lane pass may run before it
// writes a slow-pass line. Order dispatch passes run in seconds; a pass past a
// minute is holding every cooldown order behind it, so the line names the step
// that held it.
const lanePassSlowThreshold = time.Minute

// lanePhase is one timed step of a background lane pass.
type lanePhase struct {
	name string
	took time.Duration
}

// recordLanePhases records each step of a pass as a phase operation on the
// pass's trace cycle, so `gc trace` attributes a long pass to its step.
func recordLanePhases(trace *sessionReconcilerTraceCycle, phases []lanePhase) {
	if trace == nil {
		return
	}
	for _, p := range phases {
		trace.RecordControllerOperation(TraceSiteControllerTickPhase, TraceReasonRetained, TraceOutcomeComplete,
			p.name, p.took, nil)
	}
}

// logSlowLanePass writes one stderr line when a lane pass ran past
// lanePassSlowThreshold, with every step's time, so the step that held the
// lane is in the supervisor log without arming a trace.
func (cr *CityRuntime) logSlowLanePass(lane, reason string, took time.Duration, phases []lanePhase) {
	if took < lanePassSlowThreshold || cr.stderr == nil {
		return
	}
	parts := make([]string, 0, len(phases))
	for _, p := range phases {
		parts = append(parts, fmt.Sprintf("%s=%s", p.name, p.took.Round(time.Millisecond)))
	}
	fmt.Fprintf(cr.stderr, "%s: lane %s: slow pass (%s) took %s (threshold %s): %s\n", //nolint:errcheck // best-effort stderr
		cr.logPrefix, lane, reason, took.Round(time.Millisecond), lanePassSlowThreshold, strings.Join(parts, " "))
}

// startPacedLane starts a background lane goroutine and returns a channel
// closed when it exits (ctx done). pass runs one pass; wake reports whether a
// wake, rather than the backstop, triggered it.
//
// Two rules pace the lane, both measured from the end of the previous pass:
//
//   - Duty cycle. A wake starts a pass at once if the lane has been idle at
//     least as long as its previous pass ran (and at least minWakeGap).
//     Otherwise the wake waits until it has, and every wake in that wait
//     (including one that lands mid-pass) joins the one pass that follows.
//     The lane is therefore busy at most half the time while a pass fits in
//     the interval (a longer pass is followed by one interval idle), never
//     runs two passes back to back, and a lane with short passes keeps the
//     wake source's latency.
//   - Backstop. One timer, reset at the end of every pass, runs a pass one
//     interval after the previous pass ended if nothing else has. A wake never
//     waits longer than the backstop would.
//
// A free-running ticker would add passes on its own grid; a wake that ignored
// the duty cycle would run passes back to back.
func startPacedLane(ctx context.Context, interval, minWakeGap time.Duration, wakeCh <-chan struct{}, pass func(wake bool)) <-chan struct{} {
	return startGatedPacedLane(ctx, interval, minWakeGap, wakeCh, func(wake bool) bool {
		pass(wake)
		return true
	})
}

// startGatedPacedLane is startPacedLane for a pass that may decline to run:
// pass reports whether it ran, and a declined pass does not count toward the
// duty cycle, so the wake that later runs it is not paced against a pass that
// did nothing. The backstop restarts either way.
func startGatedPacedLane(ctx context.Context, interval, minWakeGap time.Duration, wakeCh <-chan struct{}, pass func(wake bool) (ran bool)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(interval)
		defer timer.Stop()
		pace := lanePace{minGap: minWakeGap}
		wakePending := false
		for {
			wake := false
			select {
			case <-ctx.Done():
				return
			case <-wakeCh:
				if wait := pace.wakeWait(time.Now(), interval); wait > 0 {
					if !wakePending {
						wakePending = true
						timer.Reset(wait)
					}
					continue
				}
				wake = true
			case <-timer.C:
				wake = wakePending
			}
			wakePending = false
			start := time.Now()
			if pass(wake) {
				pace.lastEnd, pace.lastRun, pace.passed = time.Now(), time.Since(start), true
			}
			// Go 1.23+ timers: Reset discards any pending fire, so a timer that
			// expired during the pass does not start one straight away.
			timer.Reset(interval)
		}
	}()
	return done
}

// lanePace is what a paced lane remembers of its previous pass.
type lanePace struct {
	minGap  time.Duration
	lastEnd time.Time
	lastRun time.Duration
	passed  bool
}

// wakeWait is how long a wake at now must wait before its pass may start:
// zero once the lane has idled as long as its previous pass ran (and at least
// minGap), and never longer than the backstop, which fires interval after
// that pass ended.
func (p lanePace) wakeWait(now time.Time, interval time.Duration) time.Duration {
	if !p.passed {
		return 0
	}
	gap := max(p.lastRun, p.minGap)
	if gap > interval {
		gap = interval
	}
	if wait := p.lastEnd.Add(gap).Sub(now); wait > 0 {
		return wait
	}
	return 0
}
