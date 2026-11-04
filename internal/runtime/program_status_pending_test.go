package runtime_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// recordID is the record version s carries.
func recordID(s runtime.ProgramStatus) runtime.ProgramRecordID {
	return runtime.ProgramRecordID{Nonce: s.Nonce, Seq: s.Seq}
}

// Kills a watch that misses a close (a capture showing none after one
// showing a dialog), stamps a close without an open seen first, lets a
// stale capture (taken before the latest observed one) move it, or keeps
// any record but the valid one read with the latest capture that showed a
// dialog.
func TestProgramDialogWatchObservesCloses(t *testing.T) {
	t0 := shadowNow
	blocked := live(runtime.ProgramBlocked, 3, 1)
	changed := live(runtime.ProgramBlocked, 4, 1)
	var w runtime.ProgramDialogWatch
	w = w.Observe(t0, false, blocked)
	if !w.ClosedAt.IsZero() || w.Open || w.Seen != (runtime.ProgramRecordID{}) {
		t.Fatalf("no dialog seen: %+v, want no close and no record", w)
	}
	w = w.Observe(t0.Add(time.Second), true, blocked)
	if !w.Open || !w.ClosedAt.IsZero() || w.Seen != recordID(blocked) {
		t.Fatalf("dialog seen: %+v, want open, no close and the record read with it", w)
	}
	// Every capture showing the dialog keeps the record read with it.
	w = w.Observe(t0.Add(1500*time.Millisecond), true, changed)
	if w.Seen != recordID(changed) {
		t.Fatalf("dialog seen again: %+v, want the record read with the latest capture", w)
	}
	// A capture taken before the latest observed one changes nothing.
	if got := w.Observe(t0.Add(500*time.Millisecond), false, blocked); got != w {
		t.Fatalf("stale capture moved the watch: %+v, want %+v", got, w)
	}
	w = w.Observe(t0.Add(2*time.Second), false, blocked)
	if w.Open || !w.ClosedAt.Equal(t0.Add(2*time.Second)) || w.Seen != recordID(changed) {
		t.Fatalf("dialog gone: %+v, want a close at the capture and the record the dialog was seen with", w)
	}
	// A later capture still showing none keeps the close where it was seen.
	w = w.Observe(t0.Add(3*time.Second), false, blocked)
	if !w.ClosedAt.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("second empty capture: %+v, want the close kept at the first", w)
	}
	// A zero capture time is no observation.
	if got := w.Observe(time.Time{}, true, blocked); got != w {
		t.Fatalf("zero capture time moved the watch: %+v", got)
	}
	// A dialog seen with no valid record read (a reading that is not valid
	// carries the armed nonce, never a record) keeps none.
	lost := runtime.ProgramStatus{Mode: runtime.ProgramStatusShadow, Validity: runtime.ProgramStatusReaderLost, Nonce: blocked.Nonce}
	if got := w.Observe(t0.Add(4*time.Second), true, lost); got.Seen != (runtime.ProgramRecordID{}) {
		t.Fatalf("dialog seen with a reader-lost reading: %+v, want no record", got)
	}
}

// Kills a fresh pending verdict that lets a blocked record stand once the
// parsers observed the dialog they saw it with close, or that supersedes any
// other record: one changed since (another dialog's blocked report keeps
// the root's Since and moves Seq), a new incarnation's (a new Nonce, the
// same Seq), one the dialog was seen with no valid record for, one the
// parsers never saw a dialog with, or one whose capture, or the watch's
// latest capture (a look that read nothing), still shows a dialog.
func TestProgramFreshPendingVerdict(t *testing.T) {
	t0 := shadowNow.Add(-500 * time.Millisecond)
	blocked := live(runtime.ProgramBlocked, 3, 1) // Since = shadowNow - 1 s
	blocked.Kind = runtime.ProgramBlockPermission
	otherDialog := blocked
	otherDialog.Kind, otherDialog.Seq = runtime.ProgramBlockAuth, 4
	respawned := blocked
	respawned.Nonce = "r2.%3"
	lost := runtime.ProgramStatus{Mode: runtime.ProgramStatusShadow, Validity: runtime.ProgramStatusReaderLost, Nonce: blocked.Nonce}

	var empty runtime.ProgramDialogWatch
	never := empty.Observe(t0, false, blocked)
	open := empty.Observe(t0, true, blocked)
	closed := open.Observe(t0.Add(100*time.Millisecond), false, blocked)
	reopened := closed.Observe(t0.Add(200*time.Millisecond), true, blocked)
	unseen := empty.Observe(t0, true, lost).Observe(t0.Add(100*time.Millisecond), false, blocked)
	cases := []struct {
		name string
		r    runtime.ProgramPendingReading
		want runtime.ProgramVerdict
	}{
		{"blocked, the parsers never saw a dialog", runtime.ProgramPendingReading{Status: blocked, Dialogs: never}, runtime.ProgramVerdictPending},
		{"blocked, the parsers see the dialog", runtime.ProgramPendingReading{Status: blocked, ParserPending: true, Dialogs: open}, runtime.ProgramVerdictPending},
		{"blocked, the dialog it was seen with closed", runtime.ProgramPendingReading{Status: blocked, Dialogs: closed}, runtime.ProgramVerdictNone},
		{"blocked for another dialog since (Seq moved, Since kept)", runtime.ProgramPendingReading{Status: otherDialog, Dialogs: closed}, runtime.ProgramVerdictPending},
		{"blocked under a new incarnation", runtime.ProgramPendingReading{Status: respawned, Dialogs: closed}, runtime.ProgramVerdictPending},
		{"blocked, the dialog seen with no valid record", runtime.ProgramPendingReading{Status: blocked, Dialogs: unseen}, runtime.ProgramVerdictPending},
		{"blocked, the parsers see a dialog again", runtime.ProgramPendingReading{Status: blocked, ParserPending: true, Dialogs: reopened}, runtime.ProgramVerdictPending},
		{"blocked, the watch's latest capture shows a dialog", runtime.ProgramPendingReading{Status: blocked, Dialogs: reopened}, runtime.ProgramVerdictPending},
		{"working, close observed", runtime.ProgramPendingReading{Status: live(runtime.ProgramWorking, 3, 1), Dialogs: closed}, runtime.ProgramVerdictNone},
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
