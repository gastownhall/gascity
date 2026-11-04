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

// holdRead takes one fresh hold reading of session's agent pane: one
// list-panes of the session folded to its agent pane, bounded by
// shadowReadTimeout. ok is false, and nothing is read, unless
// the switch is on; it is false as well when the read failed, the agent
// pane cannot be named, or the pane does not declare the hold. The record is
// judged under the switch with the tap liveness of the state cache's last
// process table, and the activity has gc's own keystroke echo discounted.
func (t *Tmux) holdRead(session string) (runtime.ProgramHoldReading, bool) {
	mode := t.programStatusMode()
	if mode != runtime.ProgramStatusOn || !validSessionNameRe.MatchString(session) {
		return runtime.ProgramHoldReading{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), shadowReadTimeout)
	defer cancel()
	out, err := t.runCtx(ctx, "list-panes", "-s", "-t", paneTarget(session), "-F", programHoldFormat)
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
	if !named || !pane.holdDeclared() {
		return runtime.ProgramHoldReading{}, false
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
	reading := runtime.ProgramHoldReading{Status: judgeProgramPane(mode, t.captureGatePassed(), pane, true, taps)}
	if secs, err := strconv.ParseInt(activity, 10, 64); err == nil && secs > 0 {
		reading.Activity = t.discountedActivity(session, time.Unix(secs, 0))
	}
	return reading, true
}

// ProgramStatusHoldRead implements [runtime.ProgramStatusHoldProvider]: one
// fresh hold reading of session name's agent pane (holdRead), past the
// state cache.
func (p *Provider) ProgramStatusHoldRead(name string) (runtime.ProgramHoldReading, bool) {
	return p.tm.holdRead(name)
}

var _ runtime.ProgramStatusHoldProvider = (*Provider)(nil)

// declareHold sets pane's hold declaration to hold, guarded by the nonce
// heal read on it: tmux runs the change only while the pane still carries
// that nonce, so a pane re-armed or disarmed in between keeps the
// declaration its own arm gave it.
func (t *Tmux) declareHold(pane healPane, hold bool) error {
	change := []string{"set-option", "-pu", "-t", pane.target, programHoldOption}
	if hold {
		change = []string{"set-option", "-p", "-F", "-t", pane.target, programHoldOption, "#{pane_id}"}
	}
	guard := "#{==:#{" + programTapOption + "}," + pane.tap + "}"
	if _, err := t.run("if-shell", "-F", "-t", pane.target, guard, tmuxCommandLines([][]string{change})); err != nil {
		return fmt.Errorf("declaring the hold of %s: %w", pane.target, err)
	}
	return nil
}
