package runtime

import "time"

// The record's side of a pending decision (Pending, the typing gate). A
// blocked record says a dialog owns the program's input, but a record lags
// the dialog it reports: after a dialog is answered the record can stay
// blocked for seconds, and a reading from a periodic snapshot lags it
// further. So the record's pending verdict is judged only on a reading of
// the pane's record taken fresh, together with the screen capture the
// parsers judged ([ProgramPendingReading]), never on a cached one; and a
// dialog close the parsers observed supersedes the blocked record they saw
// the dialog with until that record changes
// ([ProgramPendingReading.CloseSupersedes]).

// ProgramRecordID names one version of a pane's record: the capture
// incarnation that published it (Nonce) and its Seq, which moves with every
// change to the record. The zero value names none.
type ProgramRecordID struct {
	Nonce string
	Seq   uint64
}

// programRecordID is the version of the record s carries: zero unless s is
// valid, since only a valid reading carries a record.
func programRecordID(s ProgramStatus) ProgramRecordID {
	if !s.Valid() {
		return ProgramRecordID{}
	}
	return ProgramRecordID{Nonce: s.Nonce, Seq: s.Seq}
}

// ProgramDialogWatch is the parsers' account of one session's dialogs across
// the captures they judged: whether the latest showed a dialog, the record
// read with the latest one that did, and when they last observed a dialog
// close. The zero value has observed nothing.
type ProgramDialogWatch struct {
	// LastAt is when the latest observed capture was taken.
	LastAt time.Time
	// Open reports that the latest observed capture showed a dialog.
	Open bool
	// Seen is the record read with the latest observed capture that showed
	// a dialog; zero when no valid record was read with it.
	Seen ProgramRecordID
	// ClosedAt is when the parsers last observed a dialog close: the time
	// of the first capture showing none after one that showed a dialog.
	// Zero when no close was observed.
	ClosedAt time.Time
}

// Observe returns w with one capture taken at at observed: open when the
// parsers found a dialog in it, s the record read with it (zero when none
// was read). A capture showing a dialog sets Seen to s's record; one showing
// none after one showing a dialog is an observed close, stamped at. A
// capture taken before the latest observed one is stale and changes
// nothing.
func (w ProgramDialogWatch) Observe(at time.Time, open bool, s ProgramStatus) ProgramDialogWatch {
	if at.IsZero() || at.Before(w.LastAt) {
		return w
	}
	switch {
	case open:
		w.Seen = programRecordID(s)
	case w.Open:
		w.ClosedAt = at
	}
	w.Open, w.LastAt = open, at
	return w
}

// ProgramPendingReading is one pending decision's record side: the agent
// pane's record read fresh from the runtime together with the screen capture
// the parsers judged, the parsers' verdict on that capture, and the
// session's dialog watch.
type ProgramPendingReading struct {
	// Status is the pane's record, read fresh with the capture, past any
	// snapshot [ProgramStatusProvider] answers from.
	Status ProgramStatus
	// ParserPending reports that the parsers found a dialog in the capture.
	ParserPending bool
	// Dialogs is the session's dialog watch, the capture observed into it
	// when it could be read.
	Dialogs ProgramDialogWatch
}

// CloseSupersedes reports whether a dialog close the parsers observed
// supersedes the reading's blocked record: neither the capture nor the
// latest one the parsers observed shows a dialog, they observed a dialog
// close, and the record is the one read with the latest capture that showed
// the dialog (Dialogs.Seen: the same Nonce and Seq). Any change to the
// record since, a newer blocked report for another dialog included, moves
// Seq, and a new incarnation has a new Nonce: such a record stands, and so
// does one the parsers never saw a dialog with. The record's Since cannot
// tell a newer report: it moves only when the root's state does.
func (r ProgramPendingReading) CloseSupersedes() bool {
	w := r.Dialogs
	return r.Status.Blocked() && !r.ParserPending && !w.Open && !w.ClosedAt.IsZero() &&
		w.Seen == programRecordID(r.Status)
}

// ProgramFreshPendingVerdict is the record's pending verdict on a fresh
// pending reading (pending, typing_gate): [ProgramPendingVerdict] on the
// reading's record, except that a blocked record superseded by a dialog
// close the parsers observed reads none
// ([ProgramPendingReading.CloseSupersedes]).
func ProgramFreshPendingVerdict(r ProgramPendingReading, lastActivity, now time.Time) ProgramVerdict {
	if r.CloseSupersedes() {
		return ProgramVerdictNone
	}
	return ProgramPendingVerdict(r.Status, lastActivity, now)
}
