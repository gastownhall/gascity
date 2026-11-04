package runtime

import "time"

// The record's side of a pending decision (Pending, the typing gate). A
// blocked record says a dialog owns the program's input, but a record lags
// the dialog it reports: after a dialog is answered the record can stay
// blocked for seconds, and a reading from a periodic snapshot lags it
// further. So the record's pending verdict is judged only on a reading of
// the pane's record taken fresh, together with the screen capture the
// parsers judged ([ProgramPendingReading]), never on a cached one; and a
// dialog close the parsers observed after the record went blocked supersedes
// that blocked record until the program reports anew
// ([ProgramPendingReading.CloseSupersedes]).

// ProgramDialogWatch is the parsers' account of one session's dialogs across
// the captures they judged: whether the latest showed a dialog, and when they
// last observed one close. The zero value has observed nothing.
type ProgramDialogWatch struct {
	// LastAt is when the latest observed capture was taken.
	LastAt time.Time
	// Open reports that the latest observed capture showed a dialog.
	Open bool
	// ClosedAt is when the parsers last observed a dialog close: the time
	// of the first capture showing none after one that showed a dialog.
	// Zero when no close was observed.
	ClosedAt time.Time
}

// Observe returns w with one capture taken at at observed: open when the
// parsers found a dialog in it. A capture showing none after one showing a
// dialog is an observed close, stamped at. A capture taken before the latest
// observed one is stale and changes nothing.
func (w ProgramDialogWatch) Observe(at time.Time, open bool) ProgramDialogWatch {
	if at.IsZero() || at.Before(w.LastAt) {
		return w
	}
	if w.Open && !open {
		w.ClosedAt = at
	}
	w.Open, w.LastAt = open, at
	return w
}

// ProgramPendingReading is one pending decision's record side: the agent
// pane's record read fresh from the runtime together with the screen capture
// the parsers judged, the parsers' verdict on that capture, and their last
// observed dialog close.
type ProgramPendingReading struct {
	// Status is the pane's record, read fresh with the capture, past any
	// snapshot [ProgramStatusProvider] answers from.
	Status ProgramStatus
	// ParserPending reports that the parsers found a dialog in the capture.
	ParserPending bool
	// ParserClosedAt is when the parsers last observed a dialog close
	// ([ProgramDialogWatch.ClosedAt]), the capture included; zero when none
	// was observed.
	ParserClosedAt time.Time
}

// CloseSupersedes reports whether a dialog close the parsers observed
// supersedes the reading's blocked record: the capture shows no dialog, and
// the parsers observed a dialog close after the record's root entered
// blocked (Since). A record that entered blocked after the close, a newer
// report, stands; so does one the parsers never saw a dialog for.
func (r ProgramPendingReading) CloseSupersedes() bool {
	return r.Status.Blocked() && !r.ParserPending && !r.ParserClosedAt.IsZero() &&
		r.Status.Since.Before(r.ParserClosedAt)
}

// ProgramFreshPendingVerdict is the record's pending verdict on a fresh
// pending reading (pending, typing_gate): [ProgramPendingVerdict] on the
// reading's record, except that a dialog close the parsers observed after
// the record went blocked reads none ([ProgramPendingReading.CloseSupersedes]).
func ProgramFreshPendingVerdict(r ProgramPendingReading, lastActivity, now time.Time) ProgramVerdict {
	if r.CloseSupersedes() {
		return ProgramVerdictNone
	}
	return ProgramPendingVerdict(r.Status, lastActivity, now)
}
