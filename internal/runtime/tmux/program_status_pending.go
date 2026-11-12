package tmux

import (
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The record's side of a pending read at the tmux leaf
// (runtime.ProgramPendingReading). While the switch is not off, a pending
// read pairs the parsers' capture with a fresh read of the agent pane's
// record taken right after it (programRead, past the state cache, whose
// snapshot can lag a dialog's close by its whole TTL), and keeps each
// session's dialog watch (runtime.ProgramDialogWatch) from the captures the
// parsers judged and the records read with them, so a dialog close they
// observed supersedes the blocked record they saw the dialog with until it
// changes. While the switch is off nothing here reads or keeps anything.

// programDialogWatchLimit bounds how many sessions' dialog watches one Tmux
// keeps; past it the watches start over.
const programDialogWatchLimit = 4096

// programDialogs is a Tmux's dialog watches: soft, per process.
type programDialogs struct {
	mu    sync.Mutex
	watch map[string]runtime.ProgramDialogWatch
}

// observeDialog records one parser capture of session taken at at (open when
// the parsers found a dialog in it), with s the record read with it (zero
// when none was read), and returns the session's watch with it observed.
func (t *Tmux) observeDialog(session string, at time.Time, open bool, s runtime.ProgramStatus) runtime.ProgramDialogWatch {
	d := &t.dialogs
	d.mu.Lock()
	defer d.mu.Unlock()
	w, known := d.watch[session]
	if d.watch == nil || (!known && len(d.watch) >= programDialogWatchLimit) {
		d.watch = make(map[string]runtime.ProgramDialogWatch)
	}
	w = w.Observe(at, open, s)
	d.watch[session] = w
	return w
}

// dialogWatch returns session's dialog watch as observed so far.
func (t *Tmux) dialogWatch(session string) runtime.ProgramDialogWatch {
	d := &t.dialogs
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.watch[session]
}

// pendingReading is the record's side of one parser look at session: the
// capture's verdict and s, the record read fresh with it, observed into the
// session's dialog watch at capturedAt (a clock read before the capture).
func (t *Tmux) pendingReading(session string, capturedAt time.Time, parserPending bool, s runtime.ProgramStatus) runtime.ProgramPendingReading {
	w := t.observeDialog(session, capturedAt, parserPending, s)
	return runtime.ProgramPendingReading{Status: s, ParserPending: parserPending, Dialogs: w}
}

// pendingWithRecord is Pending with the record's side of the read: the
// parsers' answer and error exactly as Tmux.Pending gives them, and, while
// the switch is not off and the capture succeeded, the session's record read
// fresh right after the capture, paired with it (ok true). With the switch
// off it is Tmux.Pending alone: it reads and keeps nothing more.
func (p *Provider) pendingWithRecord(name string) (*runtime.PendingInteraction, runtime.ProgramPendingReading, bool, error) {
	if p.tm.programStatusMode() == runtime.ProgramStatusOff {
		pending, err := p.tm.Pending(name)
		return pending, runtime.ProgramPendingReading{}, false, err
	}
	capturedAt := time.Now()
	pending, err := p.tm.Pending(name)
	if err != nil {
		return pending, runtime.ProgramPendingReading{}, false, err
	}
	s, ok := p.tm.programRead(name)
	if !ok {
		p.tm.observeDialog(name, capturedAt, pending != nil, runtime.ProgramStatus{})
		return pending, runtime.ProgramPendingReading{}, false, nil
	}
	return pending, p.tm.pendingReading(name, capturedAt, pending != nil, s), true, nil
}
