package tmux

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The mid-turn hold at the tmux leaf (runtime.ProgramStatusHoldProvider). A
// pane armed for a provider declaring hold_mid_turn carries the hold option,
// its value bound to the pane's own id as the tap nonce is, so a value set
// at window, session or global scope declares nothing for this pane. Under
// the switch on, a typing path and the controller's queued delivery read the
// pane fresh, its capture fields, the hold option and its window's activity
// in one tmux command, and hold while the program's own record says working
// or blocked (runtime.ProgramHolds). Under off or shadow nothing is read.

// programHoldOption is the pane option a hold-declaring arm sets to the
// pane's id (set-option -F "#{pane_id}").
const programHoldOption = "@gc_ps_hold"

// programHoldFormat is the hold read's row of one pane: pane_dead, the
// capture fields (programPaneFormat), the sanitized hold option and the
// pane's window activity, terminated against the executor's trim.
const programHoldFormat = "#{pane_dead}\t" + programPaneFormat + "\t" +
	"#{s/" + programReadSanitizer + "//:" + programHoldOption + "}\t#{window_activity}\t" + runtimeInventoryEnd

// programHoldFields is the field count of programHoldFormat.
const programHoldFields = 1 + programPaneFields + 2 + 1

// programHoldRow is one pane's hold-read row: its capture fields, the hold
// option as read, and its window's activity.
type programHoldRow struct {
	pane     programPane
	hold     string
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
	return programHoldRow{pane: pane, hold: fields[1+programPaneFields], activity: fields[2+programPaneFields]}, true
}

// holdRead takes one fresh hold reading of session's agent pane: a
// display-message of target, the agent pane a nudge already resolved, or,
// with no target, a list-panes of the session folded to its agent pane. It
// is bounded by shadowReadTimeout. ok is false, and nothing is read, unless
// the switch is on; it is false as well when the read failed, the agent
// pane cannot be named, or the pane does not declare the hold. The record is
// judged under the switch with the tap liveness of the state cache's last
// process table, and the activity has gc's own keystroke echo discounted.
func (t *Tmux) holdRead(session, target string) (runtime.ProgramHoldReading, bool) {
	mode := t.programStatusMode()
	if mode != runtime.ProgramStatusOn {
		return runtime.ProgramHoldReading{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), shadowReadTimeout)
	defer cancel()
	var out string
	var err error
	switch {
	case target != "":
		out, err = t.runCtx(ctx, "display-message", "-p", "-t", target, programHoldFormat)
	case validSessionNameRe.MatchString(session):
		out, err = t.runCtx(ctx, "list-panes", "-s", "-t", paneTarget(session), "-F", programHoldFormat)
	default:
		return runtime.ProgramHoldReading{}, false
	}
	if err != nil {
		return runtime.ProgramHoldReading{}, false
	}
	var rows []programHoldRow
	for _, line := range strings.Split(out, "\n") {
		row, ok := parseProgramHoldRow(line)
		if !ok {
			return runtime.ProgramHoldReading{}, false
		}
		rows = append(rows, row)
	}
	panes := make([]programPane, len(rows))
	for i, row := range rows {
		panes[i] = row.pane
	}
	pane, named := foldProgramPane(panes)
	if !named {
		return runtime.ProgramHoldReading{}, false
	}
	var row programHoldRow
	for _, r := range rows {
		if r.pane.id == pane.id {
			row = r
		}
	}
	if row.hold != pane.id {
		return runtime.ProgramHoldReading{}, false
	}
	var taps programTaps
	if t.shadow.taps != nil {
		taps = t.shadow.taps()
	}
	reading := runtime.ProgramHoldReading{Status: judgeProgramPane(mode, t.captureGatePassed(), pane, true, taps)}
	if secs, err := strconv.ParseInt(row.activity, 10, 64); err == nil && secs > 0 {
		reading.Activity = t.discountedActivity(session, time.Unix(secs, 0))
	}
	return reading, true
}

// refuseHeldInput returns the typing refusal (runtime.ProgramHoldRefusal)
// when session's agent pane, target when the caller resolved it, declares
// the hold and its fresh record holds input now; nil otherwise, having read
// nothing unless the switch is on.
func (t *Tmux) refuseHeldInput(session, target string) error {
	r, ok := t.holdRead(session, target)
	if !ok || !runtime.ProgramHolds(r, time.Now()) {
		return nil
	}
	return runtime.ProgramHoldRefusal(session, r.Status)
}

// ProgramStatusHoldRead implements [runtime.ProgramStatusHoldProvider]: one
// fresh hold reading of session name's agent pane (holdRead), past the
// state cache.
func (p *Provider) ProgramStatusHoldRead(name string) (runtime.ProgramHoldReading, bool) {
	return p.tm.holdRead(name, "")
}

var _ runtime.ProgramStatusHoldProvider = (*Provider)(nil)
