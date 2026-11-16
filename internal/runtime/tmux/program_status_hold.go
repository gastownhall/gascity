package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The mid-turn hold at the tmux leaf (runtime.ProgramStatusHoldProvider). A
// pane armed for a provider declaring hold_mid_turn carries the hold option,
// its value bound to the pane's own id as the tap nonce is, so a value set
// at window, session or global scope declares nothing for this pane; heal
// converges it on a live capture whose declaration changed. Under the
// switch on, a starved queue's bypass reads the pane fresh
// (runtime.ProgramStatusHoldProvider): its capture fields (the hold option
// among them) and its window's activity (#{window_activity}: tmux keeps no
// per-pane activity) in one tmux command. Under off or shadow nothing is
// read.

// programHoldOption is the pane option a hold-declaring arm sets to the
// pane's id (set-option -F "#{pane_id}").
const programHoldOption = "@gc_ps_hold"

// programHoldFormat is the hold read's row of one pane: pane_dead, the
// capture fields (programPaneFormat) and the pane's window activity,
// terminated against the executor's trim.
const programHoldFormat = "#{pane_dead}\t" + programPaneFormat + "\t#{window_activity}\t" + runtimeInventoryEnd

// programHoldFields is the field count of programHoldFormat.
const programHoldFields = 1 + programPaneFields + 1 + 1

// programHoldRow is one pane's hold-read row: its capture fields and its
// window's activity.
type programHoldRow struct {
	pane     programPane
	activity string
}

// parseProgramHoldRow parses one programHoldFormat row.
func parseProgramHoldRow(line string) (programHoldRow, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) != programHoldFields || fields[len(fields)-1] != runtimeInventoryEnd {
		return programHoldRow{}, false
	}
	pane, ok := parseProgramPane(fields[1:1+programPaneFields], fields[0] == "1")
	if !ok {
		return programHoldRow{}, false
	}
	return programHoldRow{pane: pane, activity: fields[1+programPaneFields]}, true
}

// holdRead takes one fresh hold reading of session's agent pane
// (decisionRead). ok is false, and nothing is read, unless the switch is on;
// it is false as well when the read failed, the agent pane cannot be named,
// or the pane does not declare the hold.
func (t *Tmux) holdRead(session string) (runtime.ProgramHoldReading, bool) {
	r, pane, ok := t.decisionRead(session)
	if !ok || !pane.holdDeclared() {
		return runtime.ProgramHoldReading{}, false
	}
	return r, true
}

// decisionRead takes one fresh reading of session's agent pane for a
// decision under the switch on: one list-panes of the session folded to its
// agent pane, bounded by shadowReadTimeout, carrying the pane's record, its
// hold declaration and its window's activity, past the state cache's
// snapshot, with the agent pane's capture fields (its hold and settle
// declarations among them). ok is false, and nothing is read, unless the
// switch is on; it is false as well when the read failed or the agent pane
// cannot be named. The record is judged under the switch with the tap
// liveness of the state cache's last process table, and the activity has
// gc's own keystroke echo discounted.
func (t *Tmux) decisionRead(session string) (r runtime.ProgramHoldReading, pane programPane, ok bool) {
	mode := t.programStatusMode()
	if mode != runtime.ProgramStatusOn || !validSessionNameRe.MatchString(session) {
		return runtime.ProgramHoldReading{}, programPane{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), shadowReadTimeout)
	defer cancel()
	out, err := t.runCtx(ctx, "list-panes", "-s", "-t", paneTarget(session), "-F", programHoldFormat)
	if err != nil {
		return runtime.ProgramHoldReading{}, programPane{}, false
	}
	var rows []programHoldRow
	for _, line := range strings.Split(out, "\n") {
		row, ok := parseProgramHoldRow(line)
		if !ok {
			return runtime.ProgramHoldReading{}, programPane{}, false
		}
		rows = append(rows, row)
	}
	panes := make([]programPane, len(rows))
	for i, row := range rows {
		panes[i] = row.pane
	}
	pane, named := foldProgramPane(panes)
	if !named {
		return runtime.ProgramHoldReading{}, programPane{}, false
	}
	var activity string
	for _, r := range rows {
		if r.pane.id == pane.id {
			activity = r.activity
		}
	}
	var taps programTaps
	if t.shadow.taps != nil {
		taps = t.shadow.taps()
	}
	r = runtime.ProgramHoldReading{Status: judgeProgramPane(mode, t.captureGatePassed(), pane, true, taps)}
	if secs, err := strconv.ParseInt(activity, 10, 64); err == nil && secs > 0 {
		r.Activity = t.discountedActivity(session, time.Unix(secs, 0))
	}
	return r, pane, true
}

// programDecision is a deciding reading of a session's record
// (programDecide) and the clock its rule judges it at.
type programDecision struct {
	reading runtime.ProgramHoldReading
	at      time.Time
}

// programDecide takes a decision's reading under the switch on
// (decisionRead), and returns it only when it decides
// ([runtime.ProgramStatus.Decides]: valid, taken under on), stamped with the
// clock read after it: no state the reading shows can have been entered
// later, while a clock taken before the read would make a state entered
// during it look skewed and hold nothing. Under off or shadow nothing is
// read and ok is false.
func (t *Tmux) programDecide(session string) (programDecision, bool) {
	r, _, ok := t.decisionRead(session)
	if !ok || !r.Status.Decides() {
		return programDecision{}, false
	}
	return programDecision{reading: r, at: time.Now()}, true
}

// ProgramStatusHoldRead implements [runtime.ProgramStatusHoldProvider]: one
// fresh hold reading of session name's agent pane (holdRead), past the
// state cache.
func (p *Provider) ProgramStatusHoldRead(name string) (runtime.ProgramHoldReading, bool) {
	return p.tm.holdRead(name)
}

var _ runtime.ProgramStatusHoldProvider = (*Provider)(nil)

// declarePane converges pane's hold and settle declarations to want's,
// guarded by the nonce heal read on it: tmux runs the change only while the
// pane still carries that nonce, so a pane re-armed or disarmed in between
// keeps the declarations its own arm gave it.
func (t *Tmux) declarePane(pane healPane, want runtime.ProgramStatusArm) error {
	changes := appendDeclaration(nil, pane.target, programHoldOption, want.HoldMidTurn, true)
	changes = appendDeclaration(changes, pane.target, programSettleOption, want.DecideSettle, true)
	guard := "#{==:#{" + programTapOption + "}," + pane.tap + "}"
	if _, err := t.run("if-shell", "-F", "-t", pane.target, guard, tmuxCommandLines(changes)); err != nil {
		return fmt.Errorf("declaring the hold and settle of %s: %w", pane.target, err)
	}
	return nil
}
