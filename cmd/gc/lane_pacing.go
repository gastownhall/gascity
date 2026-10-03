package main

import (
	"context"
	"time"
)

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
			pass(wake)
			pace.lastEnd, pace.lastRun, pace.passed = time.Now(), time.Since(start), true
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
