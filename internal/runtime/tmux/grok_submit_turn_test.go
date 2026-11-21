package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// grokTurnPane is a Grok Build TUI for the nudge flow: a boxed composer that
// stays on screen while a turn runs, with the Ctrl+c:cancel footer below the
// box only while a turn is in progress. Its modes model the outcomes a submit
// can take:
//
//   - default: an Enter on a non-empty draft submits it — the composer clears
//     and the footer appears at once, the idle-to-busy transition the
//     verified submit confirms on;
//   - swallowEnters > 0: the composer takes that many Enters as newline
//     edits, so the drafted message stays in the box and no turn starts;
//   - drainWithoutTurn: every Enter empties the composer but starts no turn —
//     the keys reached the pane and were consumed, yet nothing the leaf can
//     read says a turn began.
type grokTurnPane struct {
	mu sync.Mutex

	session string

	swallowEnters    int
	drainWithoutTurn bool

	buffers map[string]string

	draft   string
	running bool

	events []string
}

func newGrokTurnPane(session string) *grokTurnPane {
	return &grokTurnPane{session: session}
}

// render is the pane's live screen: transcript rows, the composer box, and
// the busy footer strictly below it while a turn runs.
func (f *grokTurnPane) render() []string {
	lines := []string{"  ● grok build 1.0.50", ""}
	lines = append(lines, grokBoxTop)
	lines = append(lines, "│ ❯ "+f.draft+" │")
	lines = append(lines, grokBoxBottom)
	if f.running {
		lines = append(lines, grokHintRow)
	}
	return lines
}

func (f *grokTurnPane) execute(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "show-environment":
		if slices.Contains(args, "GC_PROVIDER") {
			return "GC_PROVIDER=grok", nil
		}
		return "", errors.New("unknown variable")
	case "display-message":
		switch {
		case slices.Contains(args, "#{pane_in_mode}"):
			return "0", nil
		case slices.Contains(args, "#{session_name}|#{session_attached}"):
			return f.session + "|0", nil
		}
		return "", nil
	case "capture-pane":
		return strings.Join(f.render(), "\n"), nil
	case "send-keys":
		f.sendKeys(args)
	case "load-buffer":
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		if f.buffers == nil {
			f.buffers = map[string]string{}
		}
		f.buffers[args[slices.Index(args, "-b")+1]] = string(data)
	case "paste-buffer":
		text := f.buffers[args[slices.Index(args, "-b")+1]]
		f.events = append(f.events, "paste:"+text)
		f.draft += text
	}
	return "", nil
}

func (f *grokTurnPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *grokTurnPane) sendKeys(args []string) {
	i := slices.Index(args, "send-keys")
	literal := false
	var keys []string
	for i++; i < len(args); i++ {
		switch args[i] {
		case "-t":
			i++
		case "-l":
			literal = true
		case "-X":
			return // copy-mode command, not input
		default:
			keys = append(keys, args[i])
		}
	}
	if literal {
		text := strings.Join(keys, " ")
		f.events = append(f.events, "type:"+text)
		f.draft += text
		return
	}
	for _, key := range keys {
		f.events = append(f.events, "key:"+key)
		switch key {
		case "C-u":
			f.draft = ""
		case "Enter":
			switch {
			case f.swallowEnters > 0:
				f.swallowEnters--
				f.draft += "\n"
			case f.drainWithoutTurn:
				f.draft = ""
			case !f.running && f.draft != "":
				f.draft = ""
				f.running = true
			}
		}
	}
}

// composer returns what sits typed in the pane's composer.
func (f *grokTurnPane) composer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.draft
}

// keysPressed returns the named keys the pane received, in order.
func (f *grokTurnPane) keysPressed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for _, event := range f.events {
		if key, ok := strings.CutPrefix(event, "key:"); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

func (f *grokTurnPane) turnRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

const grokNudgeMarker = "Reply with GROK_SUBMIT_MARKER. Do not use tools or change files."

// runGrokNudge delivers the marker message through NudgeSession to a grok pane
// set up by setup, inside a synctest bubble so every wait takes virtual time
// only. The runtime dir is created outside the bubble so its diagnostics
// outlive it for inspection.
func runGrokNudge(t *testing.T, session string, setup func(*grokTurnPane)) (*grokTurnPane, string, error) {
	t.Helper()
	// The bubble creates this session's nudge lock channel; drop it so
	// nothing outside the bubble ever touches it.
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	runtimeDir := t.TempDir()
	var pane *grokTurnPane
	var err error
	synctest.Test(t, func(_ *testing.T) {
		pane = newGrokTurnPane(session)
		setup(pane)
		cfg := DefaultConfig()
		cfg.RuntimeDir = runtimeDir
		tm := &Tmux{cfg: cfg, exec: pane}
		err = tm.NudgeSession(session, grokNudgeMarker)
	})
	return pane, runtimeDir, err
}

// assertNoUnconfirmedLog fails when runtimeDir holds a nudge-unconfirmed.log:
// that artifact is the best-effort path's, and a verified submit never writes
// it.
func assertNoUnconfirmedLog(t *testing.T, runtimeDir string) {
	t.Helper()
	var found string
	_ = filepath.WalkDir(runtimeDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "nudge-unconfirmed.log" {
			found = path
		}
		return nil
	})
	if found != "" {
		t.Errorf("runtime dir holds %s: the verified submit must not record the best-effort artifact", found)
	}
}

// A nudge to an idle grok pane submits and confirms on the turn footer: the
// Enter starts the turn, the Ctrl+c:cancel row below the composer reads busy,
// and delivery is confirmed with exactly one submit sequence — no re-sends,
// and none of the best-effort path's diagnostics.
func TestGrokSubmitIsConfirmedOnTheTurnFooter(t *testing.T) {
	pane, runtimeDir, err := runGrokNudge(t, "grok-lab", func(*grokTurnPane) {})
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil", err)
	}
	if !pane.turnRunning() {
		t.Error("the pane is not running a turn: the submit did not start one")
	}
	enters := 0
	for _, key := range pane.keysPressed() {
		if key == "Enter" {
			enters++
		}
	}
	if enters != 1 {
		t.Errorf("Enter keys = %d, want exactly 1: the footer confirms the first submit, no re-send is needed", enters)
	}
	if composer := pane.composer(); composer != "" {
		t.Errorf("composer = %q, want the submitted message gone", composer)
	}
	assertNoUnconfirmedLog(t, runtimeDir)
}

// A grok composer that never goes busy and keeps the drafted message reports
// the submit unconfirmed: the message may still sit unsubmitted in the box, so
// the error wraps ErrNudgeSubmitUnconfirmed — leaving the queue to retry it —
// and no turn may have started.
func TestGrokSubmitThatNeverConfirmsIsAnError(t *testing.T) {
	pane, runtimeDir, err := runGrokNudge(t, "grok-stuck", func(p *grokTurnPane) { p.swallowEnters = 10 })
	if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("NudgeSession = %v, want ErrNudgeSubmitUnconfirmed", err)
	}
	if !strings.Contains(pane.composer(), "GROK_SUBMIT_MARKER") {
		t.Errorf("composer = %q, want the drafted message still in the box", pane.composer())
	}
	if pane.turnRunning() {
		t.Error("the pane is running a turn: a swallowed submit starts none")
	}
	assertNoUnconfirmedLog(t, runtimeDir)
}

// A grok composer that empties without any busy reading is delivery the leaf
// observed but could not confirm: the Enter reached the pane and the pane
// consumed it, so the error wraps ErrNudgeSubmitDeliveredUnobserved — proven
// delivery, not an unconfirmed submit the queue would retry and duplicate.
func TestGrokSubmitThatDrainsUnobservedIsDelivered(t *testing.T) {
	pane, runtimeDir, err := runGrokNudge(t, "grok-drain", func(p *grokTurnPane) { p.drainWithoutTurn = true })
	if !errors.Is(err, ErrNudgeSubmitDeliveredUnobserved) {
		t.Fatalf("NudgeSession = %v, want ErrNudgeSubmitDeliveredUnobserved", err)
	}
	if composer := pane.composer(); composer != "" {
		t.Errorf("composer = %q, want the box empty: the pane consumed the message", composer)
	}
	assertNoUnconfirmedLog(t, runtimeDir)
}
