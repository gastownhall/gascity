package main

import (
	"sync"
	"time"
)

// resumeClockReading is one simultaneous reading of the wall clock and the
// monotonic clock. The wall reading carries no monotonic component; mono is
// the time since an arbitrary fixed origin on the clock that does not advance
// while the host is suspended.
type resumeClockReading struct {
	wall time.Time
	mono time.Duration
}

// resumeClock supplies the detector's readings. It is a seam because
// clock.Fake cannot drive the detector: a suspend is a wall advance with no
// monotonic advance, and the fake carries one clock.
type resumeClock func() resumeClockReading

// newSystemResumeClock returns the production resumeClock: wall time from
// time.Now with its monotonic reading stripped, and monotonic time measured
// against the instant the clock was built.
func newSystemResumeClock() resumeClock {
	start := time.Now()
	return func() resumeClockReading {
		now := time.Now()
		return resumeClockReading{wall: now.Round(0), mono: now.Sub(start)}
	}
}

// activityFloor is the earliest instant a persisted or provider-reported
// wall-clock activity stamp counts from. After a host suspend every such
// stamp is as old as the suspend was long, and a decision that compares
// "now minus stamp" against a timeout reads the suspend as idle time. Raising
// the stamp to the resume instant first makes the comparison measure awake
// time since the resume. The zero value floors nothing.
//
// A floor is an immutable snapshot: it is taken once per controller pass, so
// a decision site that runs off the tick goroutine reads the pass's value and
// never races a later detection.
type activityFloor struct {
	// at is the controller's wall time at the pass that detected the latest
	// resume. It is held as a plain wall instant (Round(0)), so it compares
	// with persisted stamps by wall clock, never by a monotonic reading.
	at time.Time
}

// raise returns t, lifted to the floor when it is before it. A zero t stays
// zero: zero means "no activity known", which callers treat separately from
// "old activity", and a floor must not turn unknown into known.
func (f activityFloor) raise(t time.Time) time.Time {
	if t.IsZero() || f.at.IsZero() || !t.Before(f.at) {
		return t
	}
	return f.at
}

// resumeDetector detects a host suspend/resume between controller passes by
// comparing how far the wall clock moved with how far the monotonic clock
// moved. They agree while the host is awake; across a suspend the wall clock
// advances by the suspended time and the monotonic clock does not.
//
// observe runs once per controller pass, before any phase that reads activity.
// State is in memory, behind its own mutex, and is not reset by a config
// reload; no status file records it.
//
// Known limits:
//   - A forward NTP step larger than the threshold reads as a resume. That is
//     benign: the floor only delays an idle decision by one timeout.
//   - A controller restart that straddles a suspend is not seen, because the
//     new process has no previous reading.
//   - A backward wall step is not handled.
//   - A suspend shorter than the threshold does not fire.
//   - CLOCK_BOOTTIME minus CLOCK_MONOTONIC is a Linux-only suspend-only
//     measure; it has not been ruled on and is not used.
type resumeDetector struct {
	read resumeClock

	mu       sync.Mutex
	havePrev bool
	prev     resumeClockReading
	// resumed is the wall time of the pass that detected the latest resume;
	// zero when none. count advances once per detection so a second consumer
	// of this detector can tell a new resume from one it has already handled.
	resumed time.Time
	count   uint64
	// lastWallDelta and lastMonoDelta are the two deltas of the latest
	// detection, for the diagnostic line.
	lastWallDelta time.Duration
	lastMonoDelta time.Duration
}

// newResumeDetector builds a detector over read. It holds no previous
// reading, so its first observe never fires.
func newResumeDetector(read resumeClock) *resumeDetector {
	return &resumeDetector{read: read}
}

// observe takes one reading and reports whether it detected a resume since the
// previous observe: the wall delta exceeding the monotonic delta by strictly
// more than twice patrol. A non-positive patrol disables detection.
func (d *resumeDetector) observe(patrol time.Duration) bool {
	if d == nil {
		return false
	}
	cur := d.read()
	// Stored and compared as a plain wall instant whatever the clock hands back.
	cur.wall = cur.wall.Round(0)
	d.mu.Lock()
	defer d.mu.Unlock()
	prev, havePrev := d.prev, d.havePrev
	d.prev, d.havePrev = cur, true
	if !havePrev || patrol <= 0 {
		return false
	}
	wallDelta := cur.wall.Sub(prev.wall)
	monoDelta := cur.mono - prev.mono
	if wallDelta-monoDelta <= 2*patrol {
		return false
	}
	d.resumed = cur.wall
	d.count++
	d.lastWallDelta, d.lastMonoDelta = wallDelta, monoDelta
	return true
}

// resumedAt returns the wall time of the pass that detected the latest resume,
// or the zero time when none has been detected.
func (d *resumeDetector) resumedAt() time.Time {
	if d == nil {
		return time.Time{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resumed
}

// detections returns how many resumes have been detected.
func (d *resumeDetector) detections() uint64 {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

// lastDeltas returns the wall and monotonic deltas of the latest detection.
func (d *resumeDetector) lastDeltas() (wall, mono time.Duration) {
	if d == nil {
		return 0, 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastWallDelta, d.lastMonoDelta
}

// floor returns the activity floor as of now. A nil detector floors nothing.
func (d *resumeDetector) floor() activityFloor {
	return activityFloor{at: d.resumedAt()}
}
