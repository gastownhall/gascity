package tmux

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Stopping the background terminals an interrupted Codex turn leaves running.
//
// Codex CLI runs a turn's shell commands in "background terminals" (its
// unified-exec PTYs), and interrupting the turn ends the turn but not them.
// Live on codex-cli 0.162.1, a turn running `sh -c "sleep 90; touch
// /tmp/marker"` in the foreground was interrupted (the rollout recorded
// turn_aborted and the pane read "■ Conversation interrupted"), yet the
// command ran on and wrote the marker 90 s later. The pane said so above the
// composer:
//
//	1 background terminal running · /ps to view · /stop to close
//
// Codex's own /stop command ends them. Typed into the interrupted, idle
// composer it printed "• Stopping all background terminals." and the command
// was gone before it could write the marker.
//
// /stop closes ALL of the session's background terminals, including ones an
// earlier turn started and left running (a dev server, a watcher). That is
// the intended meaning of an explicit stop (POST /session/{id}/stop, a run
// cancel): the session's work stops, not just its conversation. It is why
// only an explicit stop sends /stop; an interrupt that delivers a replacement
// message does not.

const (
	// codexPromptPrefix starts Codex's composer row. Codex also draws the
	// user's earlier messages with it, so the live composer is the last row
	// that starts with it.
	codexPromptPrefix = "› "
	// codexStopCommand is the Codex slash command that closes the session's
	// background terminals.
	codexStopCommand = "/stop"
	// codexSlashCommandSettle is the pause between typing /stop and pressing
	// Enter. Codex takes a fast burst of keys for a paste and an Enter inside
	// that burst for a newline; after a 0.6 s pause a plain Enter ran the
	// command live. Escape is not part of the submit: it would close the
	// slash-command popup /stop opens, and Codex binds Escape on an idle
	// composer to backtracking into the previous message.
	codexSlashCommandSettle = 600 * time.Millisecond
	// codexBackgroundStopWait bounds each wait: for a still-busy pane to go
	// idle before /stop, and for the background terminals to stop after it.
	// Live, the command was gone within 15 s of /stop.
	codexBackgroundStopWait = 15 * time.Second
	// codexBackgroundStopPoll is the pause between two captures in a wait.
	codexBackgroundStopPoll = IdlePollInterval
	// codexBackgroundStatusRows is how many non-blank rows directly above the
	// live composer belong to Codex's status area, where it reports running
	// background terminals (the Working row while a turn runs, its own row
	// after an interrupt).
	codexBackgroundStatusRows = 2
)

// codexBackgroundTerminalsRe matches Codex's report of running background
// terminals, e.g. "1 background terminal running" or "2 background terminals
// running".
var codexBackgroundTerminalsRe = regexp.MustCompile(`\b[0-9]+ background terminals? running\b`)

// codexPaneReportsBackgroundTerminals reports whether Codex's status area -
// the codexBackgroundStatusRows non-blank rows above the live composer, the
// composer, and the footer below it - says background terminals are running.
// The phrase anywhere else (transcript, scrollback) does not count. readable
// is false when no composer row is on screen, so the status area cannot be
// found.
func codexPaneReportsBackgroundTerminals(lines []string) (running, readable bool) {
	composer := -1
	for i, line := range lines {
		if matchesPromptPrefix(line, codexPromptPrefix) {
			composer = i
		}
	}
	if composer < 0 {
		return false, false
	}
	start := composer
	for above := 0; start > 0 && above < codexBackgroundStatusRows; {
		start--
		if strings.TrimSpace(lines[start]) != "" {
			above++
		}
	}
	for _, line := range lines[start:] {
		if codexBackgroundTerminalsRe.MatchString(line) {
			return true, true
		}
	}
	return false, true
}

// codexBackgroundStopOutcome is what closeCodexBackgroundTerminals did.
type codexBackgroundStopOutcome int

const (
	// codexBackgroundNone: the pane reported no running background terminal,
	// or could not be read. No keys were sent.
	codexBackgroundNone codexBackgroundStopOutcome = iota
	// codexBackgroundStopped: /stop was sent and the report cleared.
	codexBackgroundStopped
	// codexBackgroundStillRunning: /stop was sent but the pane still reported
	// running background terminals when the wait ran out.
	codexBackgroundStillRunning
	// codexBackgroundBusy: the pane never read idle within the wait. No keys
	// were sent.
	codexBackgroundBusy
	// codexBackgroundDraft: the composer holds text that /stop would be
	// appended to and submitted with. No keys were sent.
	codexBackgroundDraft
)

// codexBackgroundStopOps are the side effects closeCodexBackgroundTerminals
// drives, injected so its decisions are unit-testable without tmux.
type codexBackgroundStopOps struct {
	// observe captures the pane's visible screen.
	observe func() ([]string, error)
	// composerEmpty reports whether the live composer holds no text.
	composerEmpty func() (bool, error)
	// typeStop types /stop into the composer, without submitting it.
	typeStop func() error
	// submit presses Enter.
	submit func() error
	// sleep pauses, or fails when the caller's context is done.
	sleep func(time.Duration) error
}

// closeCodexBackgroundTerminals runs after a Codex turn's interrupt boundary.
// It waits (bounded) for the pane to read idle; when the status area then
// reports running background terminals and the composer is empty, it types
// /stop, pauses codexSlashCommandSettle, presses Enter, and polls until the
// report clears or codexBackgroundStopWait runs out. /stop is sent at most
// once. A pane that reports no background terminal gets no keys.
func closeCodexBackgroundTerminals(ops codexBackgroundStopOps) (codexBackgroundStopOutcome, error) {
	lines, err := ops.observe()
	for waited := time.Duration(0); err == nil && paneContainsBusyIndicator(lines); waited += codexBackgroundStopPoll {
		if waited >= codexBackgroundStopWait {
			return codexBackgroundBusy, nil
		}
		if err := ops.sleep(codexBackgroundStopPoll); err != nil {
			return codexBackgroundNone, err
		}
		lines, err = ops.observe()
	}
	if err != nil {
		return codexBackgroundNone, fmt.Errorf("reading the pane: %w", err)
	}
	if running, _ := codexPaneReportsBackgroundTerminals(lines); !running {
		return codexBackgroundNone, nil
	}
	empty, err := ops.composerEmpty()
	if err != nil {
		return codexBackgroundNone, fmt.Errorf("reading the composer: %w", err)
	}
	if !empty {
		return codexBackgroundDraft, nil
	}
	if err := ops.typeStop(); err != nil {
		return codexBackgroundNone, fmt.Errorf("typing %s: %w", codexStopCommand, err)
	}
	if err := ops.sleep(codexSlashCommandSettle); err != nil {
		return codexBackgroundNone, err
	}
	if err := ops.submit(); err != nil {
		return codexBackgroundNone, fmt.Errorf("submitting %s: %w", codexStopCommand, err)
	}
	for waited := time.Duration(0); waited < codexBackgroundStopWait; waited += codexBackgroundStopPoll {
		if err := ops.sleep(codexBackgroundStopPoll); err != nil {
			return codexBackgroundStillRunning, err
		}
		lines, err := ops.observe()
		if err != nil {
			return codexBackgroundStillRunning, fmt.Errorf("reading the pane after %s: %w", codexStopCommand, err)
		}
		if running, readable := codexPaneReportsBackgroundTerminals(lines); readable && !running {
			return codexBackgroundStopped, nil
		}
	}
	return codexBackgroundStillRunning, nil
}

// StopBackgroundWork closes the background terminals a stopped Codex turn, or
// the background shells a stopped Claude turn (see
// claude_background_stop.go), left running in session. It implements
// [runtime.BackgroundWorkStopper]: session.Manager.StopTurn calls it once the
// interrupt boundary is reached, and a replacement-message interrupt does not,
// so a new turn leaves an earlier turn's dev server alone. Sessions that run
// neither return runtime.ErrInteractionUnsupported and get no keys.
func (t *Tmux) StopBackgroundWork(ctx context.Context, session string) error {
	switch family := sessionlog.ProviderFamily(t.providerEnv(session)); {
	case family == "codex" || (family != "claude" && t.targetLooksLikeProvider(session, "codex")):
		return t.stopCodexBackgroundTerminals(ctx, session)
	case family == "claude" || t.targetLooksLikeProvider(session, "claude"):
		return t.stopClaudeBackgroundShells(ctx, session)
	default:
		return runtime.ErrInteractionUnsupported
	}
}

// stopCodexBackgroundTerminals closes the background terminals an interrupted
// Codex turn left running in session (see closeCodexBackgroundTerminals). The
// turn is already over, so this never fails the stop: what it could not do is
// logged. Only the caller's context ending is returned. It holds the session's
// nudge lock so a nudge cannot paste into the composer between /stop and its
// Enter.
func (t *Tmux) stopCodexBackgroundTerminals(ctx context.Context, session string) error {
	if !acquireNudgeLock(session, t.cfg.NudgeLockTimeout) {
		log.Printf("tmux: left any Codex background terminals of %q running after its interrupt: nudge lock timeout", session)
		return nil
	}
	defer releaseNudgeLock(session)

	target, err := t.agentPaneTarget(session)
	if err != nil {
		log.Printf("tmux: left any Codex background terminals of %q running after its interrupt: %v", session, err)
		return nil
	}
	sleep := t.backgroundStopSleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	outcome, err := closeCodexBackgroundTerminals(codexBackgroundStopOps{
		observe: func() ([]string, error) {
			out, err := t.CaptureVisiblePane(target)
			if err != nil || out == "" {
				return nil, err
			}
			return strings.Split(out, "\n"), nil
		},
		composerEmpty: func() (bool, error) {
			lines, err := t.captureInputLines(target)
			if err != nil {
				return false, err
			}
			return paneShowsDrainedComposer(lines), nil
		},
		typeStop: func() error {
			t.cancelCopyModeIfParked(ctx, target)
			t.recordPoke(session)
			_, err := t.run("send-keys", "-t", paneTarget(target), "-l", codexStopCommand)
			return err
		},
		submit: func() error {
			_, err := t.run("send-keys", "-t", paneTarget(target), "Enter")
			return err
		},
		sleep: func(d time.Duration) error { return sleep(ctx, d) },
	})
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		return ctxErr
	}
	switch {
	case err != nil:
		log.Printf("tmux: could not stop the Codex background terminals of %q after its interrupt: %v", session, err)
	case outcome == codexBackgroundStopped:
		log.Printf("tmux: stopped the Codex background terminals of %q after its interrupt", session)
	case outcome == codexBackgroundStillRunning:
		log.Printf("tmux: Codex still reports background terminals running in %q %v after %s", session, codexBackgroundStopWait, codexStopCommand)
	case outcome == codexBackgroundBusy:
		log.Printf("tmux: left the Codex background terminals of %q running: the pane still read busy after %v", session, codexBackgroundStopWait)
	case outcome == codexBackgroundDraft:
		log.Printf("tmux: left the Codex background terminals of %q running: its composer holds a draft %s would be sent with", session, codexStopCommand)
	}
	return nil
}
