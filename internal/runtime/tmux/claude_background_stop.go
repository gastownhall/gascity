package tmux

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Stopping the background shells an interrupted Claude turn leaves running.
//
// Claude Code runs a Bash command in the background when the model passes
// run_in_background, and interrupting the turn ends the turn but not the
// command. Live on Claude Code 2.1.296, a turn that started
// `sleep 25 && printf finished > <file>` that way was stopped through POST
// /v0/city/{city}/session/{id}/stop 0.2 s after the command started; the
// command ran on and wrote the file 25 s later, and its completion notice then
// started a turn nobody asked for. The footer under the input box said so:
//
//	⏵⏵ bypass permissions on · 2 shells · ← for agents · ↓ to manage
//
// Claude's own background-task view stops them. From the idle input box, Down
// selects the footer's shells entry ("Enter to view tasks"), Enter opens the
// view (a "Background" list of the shells, or the one shell's details), and x
// stops the shell under the cursor. Live, x on each shell left no command
// running and no notice turn followed. The view does not take an x that
// arrives while it redraws after a stop, so a shell that is still listed gets
// another x.
//
// This stops ALL of the session's background shells, including ones an
// earlier turn started (a dev server, a watcher), for the same reason Codex's
// /stop does (see codex_background_stop.go): only an explicit stop calls it.

const (
	// claudeBackgroundStopWait bounds each wait: for a still-busy pane to go
	// idle, and for the background shells to stop once the view is open.
	claudeBackgroundStopWait = 15 * time.Second
	// claudeTaskViewWait bounds each wait for the screen to answer a key in
	// the background-task view: the footer selection after Down and the view
	// after Enter (live, each within 1.2 s), a stopped shell leaving the list
	// after x before it gets another (live, within 2.5 s), and the input box
	// returning after Escape.
	claudeTaskViewWait = 3 * time.Second
	// claudeBackgroundStopPoll is the pause between two captures in a wait.
	claudeBackgroundStopPoll = IdlePollInterval
	// claudeTaskStopExtraPresses is how many more x presses than shells the
	// footer reported the view may get: one dropped press per shell is
	// expected, endless ones are not.
	claudeTaskStopExtraPresses = 3
)

var (
	// claudeFooterShellsRe matches the footer's count of background shells,
	// e.g. "· 1 shell ·" or "· 2 shells · ↓ to manage". The footer is cut to
	// the pane's width, so the entry may end the row, and it starts the row
	// when no permission mode is shown before it.
	claudeFooterShellsRe = regexp.MustCompile(`(?:^|· )([0-9]+) shells?(?: ·|…|$)`)
	// claudeTaskViewActiveRe matches the background-task view's count of
	// running shells, e.g. "2 active shells".
	claudeTaskViewActiveRe = regexp.MustCompile(`^([0-9]+) active shells?\b`)
	// claudeTaskViewHintRe matches the key hints at the bottom of the
	// background-task view: "↑/↓ to select · Enter to view · x to stop · Esc
	// to close" for the list, "← to go back · Esc/Enter/Space to close · x to
	// stop" for one shell's details. They are below the pane when the view is
	// taller than it.
	claudeTaskViewHintRe = regexp.MustCompile(`\bEsc(?:/Enter/Space)? to close\b`)
)

// claudeInputBoxEnd returns the index of the rule that closes Claude Code's
// live input box (see readClaudeInput), and false when no framed input box is
// on screen.
func claudeInputBoxEnd(lines []string) (int, bool) {
	for i := len(lines) - 1; i >= 1; i-- {
		if !isDialogRule(lines[i-1]) || !matchesPromptPrefix(lines[i], DefaultReadyPromptPrefix) {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if isDialogRule(lines[j]) {
				return j, true
			}
		}
		return 0, false
	}
	return 0, false
}

// claudeFooter returns the rows under Claude Code's live input box, where its
// footer is drawn, and false when no framed input box is on screen.
func claudeFooter(lines []string) ([]string, bool) {
	end, ok := claudeInputBoxEnd(lines)
	if !ok {
		return nil, false
	}
	return lines[end+1:], true
}

// claudePaneReportsBackgroundShells returns how many background shells Claude
// Code's footer reports running. The phrase anywhere else (the transcript, a
// "Ran 3 shell commands" row) does not count. readable is false when no input
// box is on screen, so the footer cannot be found.
func claudePaneReportsBackgroundShells(lines []string) (shells int, readable bool) {
	footer, ok := claudeFooter(lines)
	if !ok {
		return 0, false
	}
	for _, line := range footer {
		if m := claudeFooterShellsRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n, true
		}
	}
	return 0, true
}

// claudeFooterOffersTaskView reports whether Down selected the footer's
// shells entry, so that Enter opens the background-task view.
func claudeFooterOffersTaskView(lines []string) bool {
	footer, ok := claudeFooter(lines)
	if !ok {
		return false
	}
	for _, line := range footer {
		if strings.Contains(line, "Enter to view tasks") {
			return true
		}
	}
	return false
}

// claudeTaskView is what the background-task view shows.
type claudeTaskView struct {
	// open is true while the view replaces the input box.
	open bool
	// canStop is true when x stops the selected shell: its hints offer it,
	// or they are below the pane while the view lists a running shell.
	canStop bool
	// running is how many shells it lists as running.
	running int
}

// readClaudeTaskView reads the background-task view from a pane capture. The
// view is open when its heading ("Background", or "Shell details" for one
// shell) sits directly under a rule with no input box below it. The view is
// cut at the bottom of the pane: live, in GC's 24-row pane a command that
// wrapped to a second row pushed the details' key hints off screen, so the
// hints are read when they are on screen and not required.
func readClaudeTaskView(lines []string) claudeTaskView {
	heading := -1
	for i := len(lines) - 1; i >= 1; i-- {
		if row := strings.TrimSpace(lines[i]); (row == "Background" || row == "Shell details") && isDialogRule(lines[i-1]) {
			heading = i
			break
		}
	}
	if end, ok := claudeInputBoxEnd(lines); heading < 0 || (ok && end > heading) {
		return claudeTaskView{}
	}
	view := claudeTaskView{open: true}
	hinted := false
	for _, line := range lines[heading+1:] {
		row := strings.TrimSpace(line)
		if m := claudeTaskViewActiveRe.FindStringSubmatch(row); m != nil {
			view.running, _ = strconv.Atoi(m[1])
		}
		if fields := strings.Fields(row); len(fields) == 2 && fields[0] == "Status:" && fields[1] == "running" {
			view.running = 1
		}
		if claudeTaskViewHintRe.MatchString(row) {
			hinted = true
			view.canStop = strings.Contains(row, "x to stop")
		}
	}
	if !hinted {
		view.canStop = view.running > 0
	}
	return view
}

// claudeBackgroundStopOutcome is what closeClaudeBackgroundShells did.
type claudeBackgroundStopOutcome int

const (
	// claudeBackgroundNone: the footer reported no running background shell,
	// or could not be read. No keys were sent.
	claudeBackgroundNone claudeBackgroundStopOutcome = iota
	// claudeBackgroundStopped: the view stopped the shells and the footer no
	// longer reports any.
	claudeBackgroundStopped
	// claudeBackgroundStillRunning: the footer still reported running shells
	// when the wait ran out.
	claudeBackgroundStillRunning
	// claudeBackgroundBusy: the pane never read idle within the wait. No keys
	// were sent.
	claudeBackgroundBusy
	// claudeBackgroundDraft: the input box holds text that Down would move
	// through instead of selecting the footer. No keys were sent.
	claudeBackgroundDraft
	// claudeBackgroundNoTaskView: Down or Enter did not bring up the
	// background-task view. No x was sent.
	claudeBackgroundNoTaskView
)

// claudeBackgroundStopOps are the side effects closeClaudeBackgroundShells
// drives, injected so its decisions are unit-testable without tmux.
type claudeBackgroundStopOps struct {
	// observe captures the pane with dim text removed (captureInputLines).
	observe func() ([]string, error)
	// press sends one key.
	press func(key string) error
	// sleep pauses, or fails when the caller's context is done.
	sleep func(time.Duration) error
}

// closeClaudeBackgroundShells runs after a Claude turn's interrupt boundary.
// It waits (bounded) for the pane to read idle; when the footer then reports
// running background shells and the input box is empty, it opens Claude's
// background-task view with Down and Enter, presses x while the view lists a
// running shell, and closes the view with Escape. Each key is sent only when
// the screen before it shows the key will do that, so none lands in the input
// box. A pane that reports no background shell gets no keys.
func closeClaudeBackgroundShells(ops claudeBackgroundStopOps) (claudeBackgroundStopOutcome, error) {
	lines, err := ops.observe()
	for waited := time.Duration(0); err == nil && paneContainsBusyIndicator(lines); waited += claudeBackgroundStopPoll {
		if waited >= claudeBackgroundStopWait {
			return claudeBackgroundBusy, nil
		}
		if err := ops.sleep(claudeBackgroundStopPoll); err != nil {
			return claudeBackgroundNone, err
		}
		lines, err = ops.observe()
	}
	if err != nil {
		return claudeBackgroundNone, fmt.Errorf("reading the pane: %w", err)
	}
	shells, _ := claudePaneReportsBackgroundShells(lines)
	if shells == 0 {
		return claudeBackgroundNone, nil
	}
	if in, ok := readClaudeInput(lines); !ok || !in.empty() {
		return claudeBackgroundDraft, nil
	}
	// waitFor polls until done reads true of a capture, for at most
	// claudeTaskViewWait.
	waitFor := func(done func([]string) bool) ([]string, bool, error) {
		for waited := time.Duration(0); ; waited += claudeBackgroundStopPoll {
			if err := ops.sleep(claudeBackgroundStopPoll); err != nil {
				return nil, false, err
			}
			lines, err := ops.observe()
			if err != nil {
				return nil, false, fmt.Errorf("reading the pane: %w", err)
			}
			if done(lines) {
				return lines, true, nil
			}
			if waited >= claudeTaskViewWait {
				return lines, false, nil
			}
		}
	}
	if err := ops.press("Down"); err != nil {
		return claudeBackgroundNone, fmt.Errorf("selecting the footer's shells: %w", err)
	}
	if _, ok, err := waitFor(claudeFooterOffersTaskView); err != nil || !ok {
		return claudeBackgroundNoTaskView, err
	}
	if err := ops.press("Enter"); err != nil {
		return claudeBackgroundNone, fmt.Errorf("opening the background-task view: %w", err)
	}
	lines, ok, err := waitFor(func(lines []string) bool { return readClaudeTaskView(lines).open })
	if err != nil || !ok {
		return claudeBackgroundNoTaskView, err
	}
	view := readClaudeTaskView(lines)
	for presses, spent := 0, time.Duration(0); view.open && view.canStop && view.running > 0; presses++ {
		if presses >= shells+claudeTaskStopExtraPresses || spent >= claudeBackgroundStopWait {
			break
		}
		if err := ops.press("x"); err != nil {
			return claudeBackgroundStillRunning, fmt.Errorf("stopping a background shell: %w", err)
		}
		before := view.running
		lines, _, err = waitFor(func(lines []string) bool {
			next := readClaudeTaskView(lines)
			return !next.open || next.running < before
		})
		if err != nil {
			return claudeBackgroundStillRunning, err
		}
		spent += claudeTaskViewWait
		view = readClaudeTaskView(lines)
	}
	if view.open {
		if err := ops.press("Escape"); err != nil {
			return claudeBackgroundStillRunning, fmt.Errorf("closing the background-task view: %w", err)
		}
	}
	lines, _, err = waitFor(func(lines []string) bool {
		_, readable := claudePaneReportsBackgroundShells(lines)
		return readable && !readClaudeTaskView(lines).open
	})
	if err != nil {
		return claudeBackgroundStillRunning, err
	}
	if left, readable := claudePaneReportsBackgroundShells(lines); readable && left == 0 {
		return claudeBackgroundStopped, nil
	}
	return claudeBackgroundStillRunning, nil
}

// stopClaudeBackgroundShells closes the background shells an interrupted
// Claude turn left running in session (see closeClaudeBackgroundShells). The
// turn is already over, so this never fails the stop: what it could not do is
// logged. Only the caller's context ending is returned. It holds the session's
// nudge lock so a nudge cannot type into the pane between the view's keys.
func (t *Tmux) stopClaudeBackgroundShells(ctx context.Context, session string) error {
	if !acquireNudgeLock(session, t.cfg.NudgeLockTimeout) {
		log.Printf("tmux: left any Claude background shells of %q running after its interrupt: nudge lock timeout", session)
		return nil
	}
	defer releaseNudgeLock(session)

	target, err := t.agentPaneTarget(session)
	if err != nil {
		log.Printf("tmux: left any Claude background shells of %q running after its interrupt: %v", session, err)
		return nil
	}
	sleep := t.backgroundStopSleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	outcome, err := closeClaudeBackgroundShells(claudeBackgroundStopOps{
		observe: func() ([]string, error) { return t.captureInputLines(target) },
		press: func(key string) error {
			t.cancelCopyModeIfParked(ctx, target)
			t.recordPoke(session)
			_, err := t.run("send-keys", "-t", paneTarget(target), key)
			return err
		},
		sleep: func(d time.Duration) error { return sleep(ctx, d) },
	})
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		return ctxErr
	}
	switch {
	case err != nil:
		log.Printf("tmux: could not stop the Claude background shells of %q after its interrupt: %v", session, err)
	case outcome == claudeBackgroundStopped:
		log.Printf("tmux: stopped the Claude background shells of %q after its interrupt", session)
	case outcome == claudeBackgroundStillRunning:
		log.Printf("tmux: Claude still reports background shells running in %q after its background-task view", session)
	case outcome == claudeBackgroundBusy:
		log.Printf("tmux: left the Claude background shells of %q running: the pane still read busy after %v", session, claudeBackgroundStopWait)
	case outcome == claudeBackgroundDraft:
		log.Printf("tmux: left the Claude background shells of %q running: its input box holds a draft", session)
	case outcome == claudeBackgroundNoTaskView:
		log.Printf("tmux: left the Claude background shells of %q running: its background-task view did not open", session)
	}
	return nil
}
