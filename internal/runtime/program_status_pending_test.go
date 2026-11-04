package runtime_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a watch that misses a close (a capture showing none after one
// showing a dialog), stamps a close without an open seen first, or lets a
// stale capture (taken before the latest observed one) move it.
func TestProgramDialogWatchObservesCloses(t *testing.T) {
	t0 := shadowNow
	var w runtime.ProgramDialogWatch
	w = w.Observe(t0, false)
	if !w.ClosedAt.IsZero() || w.Open {
		t.Fatalf("no dialog seen: %+v, want no close", w)
	}
	w = w.Observe(t0.Add(time.Second), true)
	if !w.Open || !w.ClosedAt.IsZero() {
		t.Fatalf("dialog seen: %+v, want open and no close", w)
	}
	// A capture taken before the latest observed one changes nothing.
	if got := w.Observe(t0.Add(500*time.Millisecond), false); got != w {
		t.Fatalf("stale capture moved the watch: %+v, want %+v", got, w)
	}
	w = w.Observe(t0.Add(2*time.Second), false)
	if w.Open || !w.ClosedAt.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("dialog gone: %+v, want a close at the capture", w)
	}
	// A later capture still showing none keeps the close where it was seen.
	w = w.Observe(t0.Add(3*time.Second), false)
	if !w.ClosedAt.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("second empty capture: %+v, want the close kept at the first", w)
	}
	// A zero capture time is no observation.
	if got := w.Observe(time.Time{}, true); got != w {
		t.Fatalf("zero capture time moved the watch: %+v", got)
	}
}

// Kills a fresh pending verdict that lets a pre-close blocked record stand
// once the parsers observed the dialog close, supersedes a record that went
// blocked after the close (a newer report) or one the parsers never saw a
// dialog for, or supersedes while the parsers still see a dialog.
func TestProgramFreshPendingVerdict(t *testing.T) {
	blocked := live(runtime.ProgramBlocked, 3, 1) // Since = shadowNow - 1 s
	afterSince := blocked.Since.Add(500 * time.Millisecond)
	beforeSince := blocked.Since.Add(-500 * time.Millisecond)
	cases := []struct {
		name string
		r    runtime.ProgramPendingReading
		want runtime.ProgramVerdict
	}{
		{"blocked, no close observed", runtime.ProgramPendingReading{Status: blocked}, runtime.ProgramVerdictPending},
		{"blocked, close observed after Since", runtime.ProgramPendingReading{Status: blocked, ParserClosedAt: afterSince}, runtime.ProgramVerdictNone},
		{"blocked, close observed before Since (newer report)", runtime.ProgramPendingReading{Status: blocked, ParserClosedAt: beforeSince}, runtime.ProgramVerdictPending},
		{"blocked, close at Since", runtime.ProgramPendingReading{Status: blocked, ParserClosedAt: blocked.Since}, runtime.ProgramVerdictPending},
		{"blocked, parsers still see a dialog", runtime.ProgramPendingReading{Status: blocked, ParserPending: true, ParserClosedAt: afterSince}, runtime.ProgramVerdictPending},
		{"working, close observed", runtime.ProgramPendingReading{Status: live(runtime.ProgramWorking, 3, 1), ParserClosedAt: afterSince}, runtime.ProgramVerdictNone},
		{"stale blocked", runtime.ProgramPendingReading{Status: stale(runtime.ProgramBlocked)}, runtime.ProgramVerdictNone},
		{"unread", runtime.ProgramPendingReading{}, runtime.ProgramVerdictNone},
	}
	for _, c := range cases {
		if got := runtime.ProgramFreshPendingVerdict(c.r, time.Time{}, shadowNow); got != c.want {
			t.Errorf("%s: ProgramFreshPendingVerdict = %q, want %q", c.name, got, c.want)
		}
		if superseded := c.r.CloseSupersedes(); superseded != (c.want == runtime.ProgramVerdictNone && c.r.Status.Blocked() && c.r.Status.HoldLive(time.Time{}, shadowNow)) {
			t.Errorf("%s: CloseSupersedes = %t", c.name, superseded)
		}
	}
}
