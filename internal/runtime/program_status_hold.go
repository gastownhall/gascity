package runtime

import (
	"fmt"
	"time"
)

// The mid-turn hold (the OSC 7501 cutover's hold slice): a provider whose
// program aborts a turn that input is typed into declares it (program_status
// hold_mid_turn), and its pane is armed carrying the declaration. Under the
// switch on, a typing path and a queued delivery to such a pane each re-read
// the program's record fresh, past the state cache's snapshot, and hold
// while it says working or blocked ([ProgramHoldVerdict], the rule the
// queued_delivery shadow point measured), within the record's hold bound
// ([ProgramStatus.HoldLive]): the bound is on the record's staleness, so a
// turn that keeps drawing holds past it, and a record no activity refreshed
// holds nothing once it passes. A hold is a deferral, never a failure: the
// typing path refuses with [ProgramHoldRefusal], which every bounded caller
// refunds, and a queued delivery stays queued. Under off or shadow nothing
// is read and nothing changes; a reading that is not valid, a pane that does
// not declare the hold, and a read that fails hold nothing.

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

// ProgramHolds is the hold decision on a fresh reading r: the reading
// decides (valid, taken under the switch on) and [ProgramHoldVerdict] holds,
// judged with the pane's activity at the later of now and the clock read
// here, after the reading was taken: a caller passes the time its pass
// began, and a state the reading shows entered after that would otherwise
// read as a skewed record and hold nothing.
func ProgramHolds(r ProgramHoldReading, now time.Time) bool {
	return r.Status.Decides() && ProgramHoldVerdict(r.Status, r.Activity, laterTime(now, time.Now())) == ProgramVerdictHold
}

// ProgramRecordHold reads session name's record fresh on the leaf serving it
// ([ProgramStatusHoldProvider]) and reports whether it holds input now
// ([ProgramHolds]), with the reading it judged. A leaf without the
// capability, a switch that is not on, a pane that does not declare the
// hold, and a failed read hold nothing.
func ProgramRecordHold(p Provider, name string, now time.Time) (ProgramHoldReading, bool) {
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
	return r, ProgramHolds(r, now)
}

// ProgramHoldRefusal is the error a typing path returns for session name
// when a fresh record s holds its input. A blocked record is a dialog the
// keys would answer: the error wraps [ErrPendingInteraction], the sentinel a
// dialog the parsers see already refuses with. Any other hold (a turn or a
// child working) wraps [ErrSessionBusy]. Either way nothing was typed, and
// the caller defers the delivery without spending an attempt on it.
func ProgramHoldRefusal(name string, s ProgramStatus) error {
	if s.Blocked() {
		return fmt.Errorf("%w: session %q: its program reports a dialog open (blocked), and nothing is typed into it",
			ErrPendingInteraction, name)
	}
	state := "a turn running"
	if !s.Running() {
		state = "a child working"
	}
	return fmt.Errorf("%w: session %q: its program reports %s, and its provider declares that input typed mid-turn aborts the turn (program_status hold_mid_turn)",
		ErrSessionBusy, name, state)
}
