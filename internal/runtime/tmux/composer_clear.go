package tmux

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Clearing Claude Code's input box.
//
// Claude Code 2.1.288 deletes one wrapped row of its input box per Ctrl-U, so
// a single Ctrl-U leaves most of a long draft behind. That matters after an
// interrupt that lands before Claude's first response chunk: Claude puts the
// interrupted prompt back into the input box, and the next message pasted
// after one Ctrl-U is sent glued to what is left of the old one.
//
// Ctrl-U deletes only from the cursor back to the start of the cursor's
// wrapped row, so with the cursor at the start of the draft it deletes nothing.
// Ctrl-K deletes from the cursor to the end of its row (on an empty line it
// joins the next line). Each press here is the pair Ctrl-U Ctrl-K, which
// removes text wherever the cursor is; on Claude Code 2.1.289 it emptied
// single- and multi-line drafts with the cursor at the end, the start and the
// middle, and on an empty input box it does nothing. Moving the cursor first
// is not an option: End and Ctrl-E stop at the end of the current line, and Up
// or Down at the edge of the input box recalls prompt history into it.
//
// These helpers press the pair until the input box reads empty, and fail
// loudly when it will not. They never press Ctrl-C: on an empty input box it
// arms "press Ctrl-C again to exit".

var (
	// inputRedrawPoll is the pause between captures while waiting for the
	// input box to show the effect of a Ctrl-U.
	inputRedrawPoll = 50 * time.Millisecond
	// inputRedrawWait bounds how long one Ctrl-U may take to show.
	inputRedrawWait = 750 * time.Millisecond
	// inputRestorePoll is the pause between captures while waiting for
	// Claude to put an interrupted prompt back into the input box.
	inputRestorePoll = 100 * time.Millisecond
)

const (
	// inputMaxCtrlU caps the presses spent on one draft. Every press that
	// deletes something is progress; this only stops a runaway loop.
	inputMaxCtrlU = 200
	// inputStalledCtrlU is how many presses in a row may leave the input box
	// unchanged before the clear is declared failed. One press can delete
	// only an invisible line break.
	inputStalledCtrlU = 2
)

// claudeInput is the text of Claude Code's live input box, one entry per
// displayed row, with the prompt glyph and continuation indent removed.
type claudeInput struct {
	rows []string
}

func (in claudeInput) empty() bool {
	for _, row := range in.rows {
		if strings.TrimSpace(row) != "" {
			return false
		}
	}
	return true
}

func (in claudeInput) text() string {
	var parts []string
	for _, row := range in.rows {
		if row = strings.TrimSpace(row); row != "" {
			parts = append(parts, row)
		}
	}
	return strings.Join(parts, " ")
}

func (in claudeInput) equal(other claudeInput) bool {
	if len(in.rows) != len(other.rows) {
		return false
	}
	for i := range in.rows {
		if in.rows[i] != other.rows[i] {
			return false
		}
	}
	return true
}

// readClaudeInput finds Claude Code's live input box in a pane capture: the
// last ready-prompt row directly under a full-width rule, the wrapped rows
// below it, and the rule that closes it. Rows of the transcript start with the
// same glyph but are not framed by rules, and neither is the "❯ 1. Yes" cursor
// of a permission menu. Text drawn dim (the "Try ..." placeholder, prompt
// suggestions) must already have been removed (see stripDimText). It reports
// false when no framed input box is on screen.
func readClaudeInput(lines []string) (claudeInput, bool) {
	for i := len(lines) - 1; i >= 1; i-- {
		if !isDialogRule(lines[i-1]) || !matchesPromptPrefix(lines[i], DefaultReadyPromptPrefix) {
			continue
		}
		first, ok := lastComposerRemainder(lines[i:i+1], DefaultReadyPromptPrefix)
		if !ok {
			continue
		}
		rows := []string{strings.TrimSpace(first)}
		for _, line := range lines[i+1:] {
			if isDialogRule(line) {
				return claudeInput{rows: rows}, true
			}
			rows = append(rows, strings.TrimSpace(line))
		}
		return claudeInput{}, false
	}
	return claudeInput{}, false
}

// stripDimText removes the escape sequences from a line captured with
// capture-pane -e, and with them every character drawn with the dim (faint)
// attribute. Claude Code draws its input placeholder ("Try \"write a test for
// <filepath>\"") and prompt suggestions dim; they are not a draft, and Ctrl-U
// cannot delete them.
func stripDimText(line string) string {
	var b strings.Builder
	dim := false
	for i := 0; i < len(line); {
		if line[i] != 0x1b {
			if !dim {
				b.WriteByte(line[i])
			}
			i++
			continue
		}
		if i+1 >= len(line) {
			break
		}
		switch line[i+1] {
		case '[': // CSI: parameters, then a final byte in 0x40-0x7e.
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				dim = applySGRDim(dim, line[i+2:j])
			}
			i = j + 1
		case ']': // OSC (e.g. a hyperlink): ends with BEL or ESC \.
			j := i + 2
			for j < len(line) && line[j] != 0x07 && !strings.HasPrefix(line[j:], "\x1b\\") {
				j++
			}
			if j < len(line) && line[j] == 0x1b {
				j++
			}
			i = j + 1
		default:
			i += 2
		}
	}
	return b.String()
}

// applySGRDim returns whether text is dim after the SGR parameters params.
func applySGRDim(dim bool, params string) bool {
	if params == "" {
		return false
	}
	parts := strings.Split(params, ";")
	for k := 0; k < len(parts); k++ {
		switch parts[k] {
		case "", "0", "22":
			dim = false
		case "2":
			dim = true
		case "38", "48", "58": // extended color: 5;n or 2;r;g;b
			if k+1 < len(parts) {
				switch parts[k+1] {
				case "5":
					k += 2
				case "2":
					k += 4
				}
			}
		}
	}
	return dim
}

// readInput captures target and reads its input box.
func (t *Tmux) readInput(target string) (claudeInput, bool, error) {
	lines, err := t.captureInputLines(target)
	if err != nil {
		return claudeInput{}, false, err
	}
	in, ok := readClaudeInput(lines)
	return in, ok, nil
}

// captureInputLines captures target like CapturePaneLines, but with dim text
// removed, for callers that read the input box.
func (t *Tmux) captureInputLines(target string) ([]string, error) {
	out, err := t.run("capture-pane", "-p", "-e", "-J", "-t", paneTarget(target), "-S", fmt.Sprintf("-%d", promptObservationLines))
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		lines[i] = stripDimText(line)
	}
	return lines, nil
}

// inputClearKeys is one clearing press: Ctrl-U then Ctrl-K, sent in one
// send-keys (see the comment at the top of this file).
var inputClearKeys = []string{"C-u", "C-k"}

// emptyClaudeInput presses Ctrl-U Ctrl-K until the input box reads empty,
// starting from in. After each press it waits for the input box to redraw. It
// fails when the input box stops changing while it still holds text, when it
// can no longer be read, when press fails, or after inputMaxCtrlU presses.
func emptyClaudeInput(in claudeInput, read func() (claudeInput, bool, error), press func() error, sleep func(time.Duration)) error {
	stalled := 0
	for presses := 0; !in.empty(); presses++ {
		if presses >= inputMaxCtrlU || stalled >= inputStalledCtrlU {
			return fmt.Errorf("input box still holds %q after %d Ctrl-U Ctrl-K", firstNRunes(in.text(), 80), presses)
		}
		if err := press(); err != nil {
			return fmt.Errorf("clearing input box: %w", err)
		}
		before := in
		changed := false
		for waited := time.Duration(0); waited < inputRedrawWait; waited += inputRedrawPoll {
			sleep(inputRedrawPoll)
			next, ok, err := read()
			if err != nil {
				return fmt.Errorf("reading input box while clearing it: %w", err)
			}
			if !ok {
				return fmt.Errorf("input box disappeared while clearing %q", firstNRunes(before.text(), 80))
			}
			in = next
			if !in.equal(before) {
				changed = true
				break
			}
		}
		if changed {
			stalled = 0
		} else {
			stalled++
		}
	}
	return nil
}

// isClaudeTarget reports whether target runs Claude Code.
func (t *Tmux) isClaudeTarget(target string) bool {
	if provider := t.providerEnv(target); provider != "" {
		return sessionlog.ProviderFamily(provider) == "claude"
	}
	return t.targetLooksLikeProvider(target, "claude")
}

// clearInputBeforePaste empties the input box before a nudge pastes into it.
// The caller holds the nudge lock and has checked that no client is attached.
// For Claude it clears the whole draft and verifies it; when the input box
// cannot be read, and for other providers, it sends the single Ctrl-U it
// always has.
func (t *Tmux) clearInputBeforePaste(target string) error {
	if t.isClaudeTarget(target) {
		in, ok, err := t.readInput(target)
		if err == nil && ok {
			return t.emptyInput(target, in, nil)
		}
	}
	if _, err := t.run("send-keys", "-t", paneTarget(target), "C-u"); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return nil
}

// emptyInput clears in from target's input box. mayType, when set, runs
// before every press and stops the clear with its error.
func (t *Tmux) emptyInput(target string, in claudeInput, mayType func() error) error {
	read := func() (claudeInput, bool, error) { return t.readInput(target) }
	press := func() error {
		if mayType != nil {
			if err := mayType(); err != nil {
				return err
			}
		}
		args := append([]string{"send-keys", "-t", paneTarget(target)}, inputClearKeys...)
		_, err := t.run(args...)
		return err
	}
	return emptyClaudeInput(in, read, press, time.Sleep)
}

// noClientAttached returns an error wrapping runtime.ErrInputClearSkipped when
// a client is attached to session, or when the probe cannot tell: a human may
// be typing in the input box (#5192).
func (t *Tmux) noClientAttached(session string) error {
	attached, err := t.SessionAttachedWithError(session)
	if err != nil {
		return fmt.Errorf("%w (attachment probe failed: %v)", runtime.ErrInputClearSkipped, err)
	}
	if attached {
		return fmt.Errorf("%w: a client is attached to session %q", runtime.ErrInputClearSkipped, session)
	}
	return nil
}

// ClearInput implements [runtime.InputClearProvider] for Claude Code panes. It
// watches the input box for up to restoreWindow; as soon as it holds text it
// is cleared and verified empty. An input box that stays empty, or cannot be
// read, gets no keys. When a client is attached, or the attachment probe
// cannot tell, it sends no keys and returns an error wrapping
// runtime.ErrInputClearSkipped; it checks before every key it sends. Other
// providers return runtime.ErrInteractionUnsupported.
func (t *Tmux) ClearInput(ctx context.Context, session string, restoreWindow time.Duration) error {
	if !acquireNudgeLock(session, t.cfg.NudgeLockTimeout) {
		return fmt.Errorf("nudge lock timeout for session %q: previous nudge may be hung", session)
	}
	defer releaseNudgeLock(session)

	target := session
	if agentPane, err := t.FindAgentPane(session); err == nil && agentPane != "" {
		target = agentPane
	}
	if !t.isClaudeTarget(target) {
		return runtime.ErrInteractionUnsupported
	}
	// A detached pane may not have redrawn since the interrupt.
	t.WakePaneIfDetached(session)

	mayType := func() error { return t.noClientAttached(session) }
	deadline := time.Now().Add(restoreWindow)
	for {
		if err := mayType(); err != nil {
			return err
		}
		in, ok, err := t.readInput(target)
		if err != nil {
			return fmt.Errorf("reading input box: %w", err)
		}
		if ok && !in.empty() {
			return t.emptyInput(target, in, mayType)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		if err := sleepWithContext(ctx, min(remaining, inputRestorePoll)); err != nil {
			return err
		}
	}
}
