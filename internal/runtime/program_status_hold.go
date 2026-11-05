package runtime

import "time"

// The mid-turn hold (the OSC 7501 cutover's hold slice): a provider whose
// program aborts a turn that input is typed into declares it (program_status
// hold_mid_turn), and its pane is armed carrying the declaration
// ([ProgramStatus.HoldDeclared]). Under the switch on, the one decision
// that types into a session whose activity clock calls it busy, a starved
// queue's bypass of the quiescence gate, re-reads the program's record
// fresh, past the state cache's snapshot, and defers while the record says
// a turn is running ([ProgramTypingHolds]), judged with the pane's own
// activity within the record's hold bound ([ProgramStatus.HoldLive]). The
// bound is on the record's staleness: a turn that keeps drawing holds past
// it, and a record no activity refreshed holds nothing once it passes.
//
// Every other delivery decides as it does without the record: a program
// may report working with no turn running (Codex while a background
// terminal runs), and the activity clock already holds a running turn that
// keeps its pane drawing. Under off or shadow nothing is read; a reading
// that is not valid, a pane that does not declare the hold, and a read that
// fails hold nothing.

// ProgramHoldReading is one fresh reading taken for a hold decision: the
// agent pane's judged record and the pane's own last activity, read together.
type ProgramHoldReading struct {
	Status ProgramStatus
	// Activity is the pane's last activity, gc's own keystroke echo
	// discounted; zero when the runtime could not read it, which judges the
	// hold on the record's own age.
	Activity time.Time
}

// ProgramStatusHoldProvider is an optional extension: a leaf that reads a
// session's record fresh for a hold decision. Composites do not implement it
// ([ProgramStatusHoldFor] resolves the leaf).
type ProgramStatusHoldProvider interface {
	// ProgramStatusHoldRead reads session name's agent pane fresh, past any
	// snapshot [ProgramStatusProvider] answers from: its capture fields,
	// its hold declaration and its activity in one runtime read. Unless the
	// switch is on it reads nothing and ok is false; ok is false as well
	// when the read failed or the pane does not declare the hold.
	ProgramStatusHoldRead(name string) (r ProgramHoldReading, ok bool)
}

// ProgramStatusHoldFor returns the [ProgramStatusHoldProvider] of the backend
// serving session name, the leaf [ResolveBackend] names, and false when that
// backend takes no such reading.
func ProgramStatusHoldFor(p Provider, name string) (ProgramStatusHoldProvider, bool) {
	if p == nil {
		return nil, false
	}
	leaf, _, _ := ResolveBackend(p, name)
	h, ok := leaf.(ProgramStatusHoldProvider)
	return h, ok
}

// ProgramTypingHolds is the typing hold on a fresh reading r: the reading
// decides (valid, taken under the switch on) and [ProgramTurnHoldVerdict]
// holds (a live working root), judged with the pane's activity at the later
// of now and the clock read here, after the reading was taken: a caller
// passes the time its pass began, and a state the reading shows entered
// after that would otherwise read as a skewed record and hold nothing.
func ProgramTypingHolds(r ProgramHoldReading, now time.Time) bool {
	return r.Status.Decides() && ProgramTurnHoldVerdict(r.Status, r.Activity, laterTime(now, time.Now())) == ProgramVerdictHold
}

// ProgramRecordTypingHold reads session name's record fresh on the leaf
// serving it ([ProgramStatusHoldProvider]) and reports whether it holds
// typing now ([ProgramTypingHolds]), with the reading it judged. A leaf
// without the capability, a switch that is not on, a pane that does not
// declare the hold, and a failed read hold nothing.
func ProgramRecordTypingHold(p Provider, name string, now time.Time) (ProgramHoldReading, bool) {
	if name == "" {
		return ProgramHoldReading{}, false
	}
	h, ok := ProgramStatusHoldFor(p, name)
	if !ok {
		return ProgramHoldReading{}, false
	}
	r, ok := h.ProgramStatusHoldRead(name)
	if !ok {
		return ProgramHoldReading{}, false
	}
	return r, ProgramTypingHolds(r, now)
}
