package main

import (
	"testing"
	"time"
)

// fakeResumeClock hands the detector scripted wall and monotonic readings.
// clock.Fake cannot drive the detector: it carries no monotonic reading, so
// a suspend (wall advances, monotonic does not) has to be scripted directly.
type fakeResumeClock struct {
	wall time.Time
	mono time.Duration
}

// read is the resumeClock seam the detector takes.
func (f *fakeResumeClock) read() resumeClockReading {
	return resumeClockReading{wall: f.wall, mono: f.mono}
}

// tick advances both clocks by d, as an awake controller would see.
func (f *fakeResumeClock) tick(d time.Duration) {
	f.wall = f.wall.Add(d)
	f.mono += d
}

// suspend advances the wall clock by d without advancing the monotonic clock,
// as a host suspend does.
func (f *fakeResumeClock) suspend(d time.Duration) {
	f.wall = f.wall.Add(d)
}

func newFakeResumeClock() *fakeResumeClock {
	return &fakeResumeClock{wall: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
}

const testResumePatrol = 30 * time.Second

func TestResumeDetector_NoPreviousReadingNeverFires(t *testing.T) {
	t.Parallel()
	fc := newFakeResumeClock()
	d := newResumeDetector(fc.read)
	if d.observe(testResumePatrol) {
		t.Fatal("first observation fired; no previous reading means no detection")
	}
	if !d.resumedAt().IsZero() || d.detections() != 0 {
		t.Fatalf("after first observation resumedAt=%v detections=%d, want zero", d.resumedAt(), d.detections())
	}
}

func TestResumeDetector_NormalTickDoesNotFire(t *testing.T) {
	t.Parallel()
	fc := newFakeResumeClock()
	d := newResumeDetector(fc.read)
	d.observe(testResumePatrol)
	for i := 0; i < 5; i++ {
		fc.tick(testResumePatrol)
		if d.observe(testResumePatrol) {
			t.Fatalf("tick %d fired on a normal patrol interval", i)
		}
	}
	if d.detections() != 0 || !d.resumedAt().IsZero() {
		t.Fatalf("detections=%d resumedAt=%v, want none", d.detections(), d.resumedAt())
	}
}

func TestResumeDetector_LongSuspendFiresAndFloorsAtWallNow(t *testing.T) {
	t.Parallel()
	fc := newFakeResumeClock()
	d := newResumeDetector(fc.read)
	d.observe(testResumePatrol)
	fc.tick(testResumePatrol)
	fc.suspend(72 * time.Hour)
	if !d.observe(testResumePatrol) {
		t.Fatal("72h suspend did not fire")
	}
	if got := d.resumedAt(); !got.Equal(fc.wall) {
		t.Fatalf("resumedAt = %v, want the detecting pass's wall time %v", got, fc.wall)
	}
	if d.detections() != 1 {
		t.Fatalf("detections = %d, want 1", d.detections())
	}
	// The next normal pass must not fire again, and must keep the floor.
	fc.tick(testResumePatrol)
	if d.observe(testResumePatrol) {
		t.Fatal("pass after a resume fired again")
	}
	if d.detections() != 1 {
		t.Fatalf("detections = %d after a quiet pass, want 1", d.detections())
	}
}

func TestResumeDetector_ShortSuspendDoesNotFire(t *testing.T) {
	t.Parallel()
	fc := newFakeResumeClock()
	d := newResumeDetector(fc.read)
	d.observe(testResumePatrol)
	fc.tick(testResumePatrol)
	fc.suspend(50 * time.Second)
	if d.observe(testResumePatrol) {
		t.Fatal("a 50s suspend fired; the threshold is 2x patrol (60s)")
	}
}

func TestResumeDetector_ThresholdIsStrictlyGreaterThanTwicePatrol(t *testing.T) {
	t.Parallel()
	fc := newFakeResumeClock()
	d := newResumeDetector(fc.read)
	d.observe(testResumePatrol)
	fc.suspend(2 * testResumePatrol)
	if d.observe(testResumePatrol) {
		t.Fatal("a skew of exactly 2x patrol fired; want strictly greater")
	}
	fc.suspend(2*testResumePatrol + time.Nanosecond)
	if !d.observe(testResumePatrol) {
		t.Fatal("a skew of 2x patrol + 1ns did not fire")
	}
}

func TestResumeDetector_SecondResumeAdvancesCounterAndRaisesFloor(t *testing.T) {
	t.Parallel()
	fc := newFakeResumeClock()
	d := newResumeDetector(fc.read)
	d.observe(testResumePatrol)
	fc.suspend(time.Hour)
	d.observe(testResumePatrol)
	first := d.resumedAt()
	fc.tick(10 * time.Minute)
	fc.suspend(2 * time.Hour)
	if !d.observe(testResumePatrol) {
		t.Fatal("second resume did not fire")
	}
	if d.detections() != 2 {
		t.Fatalf("detections = %d, want 2", d.detections())
	}
	if !d.resumedAt().After(first) {
		t.Fatalf("second floor %v is not after first %v", d.resumedAt(), first)
	}
}

func TestResumeDetector_FloorSnapshotIsStableAcrossLaterResume(t *testing.T) {
	t.Parallel()
	fc := newFakeResumeClock()
	d := newResumeDetector(fc.read)
	d.observe(testResumePatrol)
	fc.suspend(time.Hour)
	d.observe(testResumePatrol)
	snap := d.floor()
	at := d.resumedAt()
	fc.suspend(time.Hour)
	d.observe(testResumePatrol)
	if got := snap.raise(at.Add(-time.Hour)); !got.Equal(at) {
		t.Fatalf("an earlier tick's snapshot moved to %v, want it pinned at %v", got, at)
	}
}

func TestActivityFloor_Raise(t *testing.T) {
	t.Parallel()
	resume := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	floor := activityFloor{at: resume}
	var none activityFloor
	cases := []struct {
		name  string
		floor activityFloor
		in    time.Time
		want  time.Time
	}{
		{"zero stays zero under a floor", floor, time.Time{}, time.Time{}},
		{"zero stays zero without a floor", none, time.Time{}, time.Time{}},
		{"no resume leaves a stale stamp alone", none, resume.Add(-72 * time.Hour), resume.Add(-72 * time.Hour)},
		{"stamp before resume is raised", floor, resume.Add(-72 * time.Hour), resume},
		{"stamp at resume is unchanged", floor, resume, resume},
		{"stamp after resume is unchanged", floor, resume.Add(time.Minute), resume.Add(time.Minute)},
	}
	for _, tc := range cases {
		if got := tc.floor.raise(tc.in); !got.Equal(tc.want) {
			t.Errorf("%s: raise(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestActivityFloor_StoredAsPlainWallInstant(t *testing.T) {
	t.Parallel()
	fc := newFakeResumeClock()
	fc.wall = time.Now() // carries a monotonic reading
	d := newResumeDetector(fc.read)
	d.observe(testResumePatrol)
	fc.suspend(time.Hour)
	d.observe(testResumePatrol)
	// Round(0) strips the monotonic reading; a floor that kept one would
	// compare against persisted wall stamps by its monotonic clock.
	if got := d.resumedAt(); got != got.Round(0) {
		t.Fatalf("resumedAt %v keeps a monotonic reading", got)
	}
}

func TestSystemResumeClock_MonotonicAdvances(t *testing.T) {
	t.Parallel()
	read := newSystemResumeClock()
	a := read()
	b := read()
	for deadline := time.Now().Add(time.Second); b.mono <= a.mono && time.Now().Before(deadline); {
		b = read()
	}
	if b.mono <= a.mono {
		t.Fatalf("monotonic reading did not advance: %v then %v", a.mono, b.mono)
	}
	if a.wall != a.wall.Round(0) {
		t.Fatalf("wall reading %v keeps a monotonic reading", a.wall)
	}
}

func TestIdleTracker_ResumeFloorMakesStaleActivityNotIdle(t *testing.T) {
	t.Parallel()
	it := newIdleTracker()
	it.setTimeout("mayor", 5*time.Minute)
	sp := newFakeIdleSnapshotProvider()
	startFakeSession(t, sp.Fake, "mayor")
	now := time.Now()
	sp.SetActivity("mayor", now.Add(-72*time.Hour))

	// Control: no resume detected, the stale stamp reads as idle.
	if !it.checkIdle("mayor", "", "", "", sp, now) {
		t.Fatal("control: 72h-old activity with no resume must read idle")
	}
	// Resume detected 10s ago: idle is measured from the resume, so the
	// session is awake-idle for 10s only.
	it.setActivityFloor(activityFloor{at: now.Add(-10 * time.Second)})
	if it.checkIdle("mayor", "", "", "", sp, now) {
		t.Fatal("72h-old activity read idle 10s after a detected resume")
	}
	if !it.checkIdle("mayor", "", "", "", sp, now.Add(5*time.Minute+time.Second)) {
		t.Fatal("session not idle once idle_timeout has elapsed past the resume")
	}
}

func TestIdleTracker_ResumeFloorClaudeSeatContentRouteUntouched(t *testing.T) {
	t.Parallel()
	it := newIdleTracker()
	it.setTimeout("deacon", time.Hour)
	sp := newFakeIdleSnapshotProvider()
	startFakeSession(t, sp.Fake, "deacon")
	sp.idle["deacon"] = false // content route negative: falls through to activity
	now := time.Now()
	sp.SetActivity("deacon", now.Add(-72*time.Hour))
	it.setActivityFloor(activityFloor{at: now.Add(-time.Minute)})
	if it.checkIdle("deacon", "", "claude", "", sp, now) {
		t.Fatal("claude seat with a negative content route stopped 1m after a resume")
	}
	// The content route anchors on the controller's own now and is not floored:
	// a pane that keeps reading idle still fires after the timeout.
	sp.idle["deacon"] = true
	if it.checkIdle("deacon", "", "claude", "", sp, now) {
		t.Fatal("content route fired on its first idle observation")
	}
	if !it.checkIdle("deacon", "", "claude", "", sp, now.Add(time.Hour+time.Second)) {
		t.Fatal("content route did not fire after a continuous idle hour; the floor must not touch it")
	}
}

func TestIdleTracker_ResumeFloorClaudeMuxSeatActivityRouteOnly(t *testing.T) {
	t.Parallel()
	it := newIdleTracker()
	it.setTimeout("mux", time.Hour)
	sp := newFakeIdleSnapshotProvider()
	startFakeSession(t, sp.Fake, "mux")
	now := time.Now()
	sp.SetActivity("mux", now.Add(-72*time.Hour))
	it.setActivityFloor(activityFloor{at: now.Add(-time.Minute)})
	if it.checkIdle("mux", "", "claude-mux", "", sp, now) {
		t.Fatal("claude-mux seat stopped 1m after a resume")
	}
}
