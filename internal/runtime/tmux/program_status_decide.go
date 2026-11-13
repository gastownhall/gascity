package tmux

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The OSC 7501 cutover decisions at the tmux leaf (ga-f4ovjm.76). Under the
// switch on, two leaf decisions the second shadow measured with the
// program's own record now decide with it, through the same pure rules the
// shadow recorded (runtime.ProgramStartupVerdict, runtime.ProgramSettleVerdict),
// each only for a provider that opts into it (program_status decide_startup,
// decide_settle; ga-f4ovjm.79): a provider that does not decides it from the
// screen alone, exactly as under off. The startup ladder runs inside the
// start, which carries the opt-in in its arm; the settle follows an
// interrupt any process may send, so its opt-in is bound to the pane
// (programSettleOption) as the hold declaration is, and heal keeps it
// current:
//
//   - the startup ladder (startup_ladder): a turn start past the baseline
//     taken before the first send confirms the startup nudge (positive
//     evidence only: a record never unconfirms what the scan confirmed), and
//     a live blocked record counts as busy: no send types into the dialog,
//     the ladder backs off, and on exhaustion the start goes on with the
//     nudge handed back for its caller to queue (design §10.2 row A20);
//   - the interrupt settle (interrupt_settle): an idle wait that follows an
//     interrupt gc sent (Interrupt's own, or keys its caller noted as one:
//     NoteInterrupt) settles at the first root boundary past the
//     pre-interrupt baseline, under the same nonce; a live blocked record is
//     no settle, and the wait returns at once with an error wrapping
//     runtime.ErrPendingInteraction, so the caller sends no more keys.
//
// Every reading is fresh (decisionRead: one list-panes of the session past
// the state cache, carrying the pane's activity), and each rule judges it at
// a clock read after it (programDecide). A reading that does not decide
// (any switch but on, a pane not armed, a record silent, cleared, lost or
// exited) leaves the scan's decision exactly as it is.

// errStartupNudgeBlocked is a startup-ladder send the program's own record
// held back: a live blocked root, a dialog the nudge would answer. Nothing
// was typed. It wraps runtime.ErrPendingInteraction; the ladder retries it
// as it does a deferral, and the start that exhausts the ladder on it goes
// on with its nudge handed back (startupNudgeHandback).
var errStartupNudgeBlocked = fmt.Errorf("%w: the program reports a dialog, so the startup nudge was not typed", runtime.ErrPendingInteraction)

// programSettleOption is the pane option a settle-declaring arm sets to the
// pane's id (set-option -F "#{pane_id}"): the pane's provider opts the
// interrupt settle into deciding with the record
// ([runtime.ProgramStatusArm.DecideSettle]). A value set at window, session
// or global scope declares nothing for this pane.
const programSettleOption = "@gc_ps_settle"

// programStartupLadder wraps the startup ladder's send and busy look to
// decide with session's record under the switch on, for a start whose arm
// opts into it (arm.DecideStartup), against a baseline read before the
// ladder's first send. Off on, or without the opt-in, both are returned as
// they are and nothing is read.
//
//   - Before each send, a live blocked record holds it: errStartupNudgeBlocked,
//     nothing typed. Once an earlier attempt typed the nudge, a dialog
//     reported after it is the program's answer to a nudge it took (design
//     §10.2 row N3): the ladder ends delivered but unobserved
//     (ErrNudgeSubmitDeliveredUnobserved), and nothing is handed back.
//   - A send the scan could not confirm (ErrNudgeSubmitUnconfirmed, or
//     delivered but never seen busy) is confirmed by a record that advanced
//     past the baseline (runtime.ProgramStartupVerdict confirmed).
//   - The busy look reads a confirmed record as busy (the nudge landed) and
//     a blocked one as not busy (the next send holds); otherwise the scan's
//     look decides.
func (t *Tmux) programStartupLadder(session string, arm runtime.ProgramStatusArm, send func() error, busy func() (bool, error)) (func() error, func() (bool, error)) {
	if !arm.DecideStartup || t.programStatusMode() != runtime.ProgramStatusOn {
		return send, busy
	}
	// A baseline that could not be read, or is not valid, credits nothing
	// (runtime.ProgramStatus.AdvancedPast); the blocked hold still holds.
	r, _, _ := t.decisionRead(session)
	baseline := r.Status.Baseline()
	verdict := func() (runtime.ProgramVerdict, bool) {
		d, ok := t.programDecide(session)
		if !ok {
			return "", false
		}
		return runtime.ProgramStartupVerdict(d.reading.Status, baseline, d.reading.Activity, d.at), true
	}
	typed := false
	decidedSend := func() error {
		if v, ok := verdict(); ok && v == runtime.ProgramVerdictPending {
			if typed {
				return fmt.Errorf("%w: session %q: the program reports a dialog after the startup nudge was typed", ErrNudgeSubmitDeliveredUnobserved, session)
			}
			return fmt.Errorf("session %q: %w", session, errStartupNudgeBlocked)
		}
		err := send()
		typed = typed || startupSendTyped(err)
		if errors.Is(err, ErrNudgeSubmitUnconfirmed) || errors.Is(err, ErrNudgeSubmitDeliveredUnobserved) {
			if v, ok := verdict(); ok && v == runtime.ProgramVerdictConfirmed {
				return nil
			}
		}
		return err
	}
	decidedBusy := func() (bool, error) {
		if v, ok := verdict(); ok {
			switch v {
			case runtime.ProgramVerdictConfirmed:
				return true, nil
			case runtime.ProgramVerdictPending:
				return false, nil
			}
		}
		if busy == nil {
			return false, nil
		}
		return busy()
	}
	return decidedSend, decidedBusy
}

// startupSendTyped reports whether a startup send that returned err typed
// the nudge into the pane: it succeeded, or its submit went out unconfirmed.
// A send a running turn held back (runtime.ErrSessionBusy, errNudgeSubmitHeld)
// or a pending interaction refused typed nothing that stands.
func startupSendTyped(err error) bool {
	return err == nil || errors.Is(err, ErrNudgeSubmitUnconfirmed) || errors.Is(err, ErrNudgeSubmitDeliveredUnobserved)
}

// programInterruptWindow bounds how long after an interrupt the leaf sent
// under the switch on an idle wait is read as that interrupt's settle
// (programSettle): the interrupting caller waits at once (a Ctrl-C, then
// its idle wait), so a note older than this belongs to an interrupt no wait
// followed, and a later, unrelated wait is the scan's.
const programInterruptWindow = 5 * time.Second

// programSettle returns the record's check on an idle wait that follows an
// interrupt gc noted for session (noteProgramInterrupt) within
// programInterruptWindow, under the switch on, and forgets the note: settled
// (true) at the first root boundary past the pre-interrupt baseline under
// its nonce, and an error wrapping runtime.ErrPendingInteraction on a live
// blocked root that moved past the baseline (the same nonce, a later Seq):
// a dialog raised since the interrupt. A blocked record the interrupt has not
// moved yet (the dialog its keys may be canceling, before the program
// reports again) decides nothing. Anything else, a reading that does not
// decide included, leaves the scan to decide (false, nil), and so does a
// reading whose pane no longer declares the settle: heal withdraws the
// declaration under the same nonce, so a wait the note armed decides as
// under off from the first reading that shows it gone. ok is false, and
// the wait is the scan's alone, when the switch is not on or no interrupt
// was noted within the window.
func (t *Tmux) programSettle(session string, at time.Time) (check func() (bool, error), ok bool) {
	if t.programStatusMode() != runtime.ProgramStatusOn {
		return nil, false
	}
	mark, noted := t.takeShadowInterrupt(session, at)
	if !noted || at.Sub(mark.at) > programInterruptWindow {
		return nil, false
	}
	return func() (bool, error) {
		d, ok := t.programDecide(session)
		if !ok || !d.reading.Status.SettleDeclared {
			return false, nil
		}
		s := d.reading.Status
		switch runtime.ProgramSettleVerdict(s, mark.baseline, d.reading.Activity, d.at) {
		case runtime.ProgramVerdictSettled:
			return true, nil
		case runtime.ProgramVerdictPending:
			if s.Nonce != mark.baseline.Nonce || s.Seq <= mark.baseline.Seq {
				return false, nil
			}
			return false, fmt.Errorf("session %q: the program reports a dialog after the interrupt, so it has not settled: %w", session, runtime.ErrPendingInteraction)
		}
		return false, nil
	}, true
}

// noteProgramInterrupt keeps session's record position as gc interrupts it
// under the switch on, for the settle wait that follows (programSettle). It
// takes one fresh read before the interrupt's keys, as the shadow does
// (shadowNoteInterrupt); a read that fails, or a pane that does not declare
// the settle (programSettleOption), notes nothing, and the wait is the
// scan's. A baseline that is not valid settles only on a later valid record
// of its nonce that changed since (runtime.ProgramSettleVerdict).
func (t *Tmux) noteProgramInterrupt(session string) {
	r, pane, ok := t.decisionRead(session)
	if !ok || !pane.settleDeclared() {
		return
	}
	t.keepInterrupt(session, shadowInterrupt{baseline: r.Status.Baseline(), at: time.Now()})
}

// NoteInterrupt implements [runtime.InterruptNoteProvider]: under the switch
// on, name's record position before the keys of an interrupt its caller
// sends itself (a soft-Escape family's Escape), taken as Interrupt takes its
// own (noteProgramInterrupt), so the idle wait after those keys is that
// interrupt's settle when the pane declares it. Off on it reads nothing; the
// shadow notes only Interrupt's own.
func (p *Provider) NoteInterrupt(name string) {
	p.tm.noteProgramInterrupt(name)
}

var _ runtime.InterruptNoteProvider = (*Provider)(nil)

// startupHandbacks holds, per session, the startup nudge a Start left
// undelivered behind a dialog its program reported (errStartupNudgeBlocked),
// until the Start's caller takes it (TakeStartupNudgeHandback). Each Start
// clears its session's entry first, so an untaken one never outlives the
// next start of that session.
type startupHandbacks struct {
	mu     sync.Mutex
	nudges map[string]string
}

// set records nudge for session; an empty nudge clears it.
func (h *startupHandbacks) set(session, nudge string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if nudge == "" {
		delete(h.nudges, session)
		return
	}
	if h.nudges == nil {
		h.nudges = make(map[string]string)
	}
	h.nudges[session] = nudge
}

// take returns and forgets session's handed-back nudge.
func (h *startupHandbacks) take(session string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	nudge, ok := h.nudges[session]
	delete(h.nudges, session)
	return nudge, ok
}

// TakeStartupNudgeHandback implements [runtime.StartupNudgeHandbackProvider]:
// the startup nudge the last Start of name could not type because its
// program reported a dialog through the whole startup ladder, once.
func (p *Provider) TakeStartupNudgeHandback(name string) (string, bool) {
	return p.tm.handbacks.take(name)
}

var _ runtime.StartupNudgeHandbackProvider = (*Provider)(nil)
