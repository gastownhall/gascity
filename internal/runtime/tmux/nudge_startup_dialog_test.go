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
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// A Codex pane can sit on its folder-trust dialog long after startup stopped
// watching it. A nudge typed into that dialog is read as menu input: Enter
// confirms whichever row is selected, Escape quits Codex, and a digit in the
// text can pick "2. Quit". So the nudge answers the dialog first, the way
// startup would, and only then types, or types nothing at all.

// fakeCodexTrustPane is a tmux executor that plays a codex-cli 0.162.1 pane
// showing its folder-trust dialog (dialog) until the dialog is answered, and
// its composer after that. Enter on the dialog trusts the folder when the
// dialog's cursor is on "Trust and continue" (trustSelected); any other key
// that reaches the dialog is recorded in dialogKeys, and Escape, or Enter off
// the trust row, quits Codex.
type fakeCodexTrustPane struct {
	mu sync.Mutex

	dialog        string
	trustSelected bool

	draft      string
	busy       bool
	quit       bool
	keys       []string // every key that reached the composer, text included
	dialogKeys []string // every key that reached the dialog, text included
	submitted  []string
}

func newFakeCodexTrustPane(t *testing.T, fixture string) *fakeCodexTrustPane {
	t.Helper()
	pane := &fakeCodexTrustPane{}
	if fixture != "" {
		data, err := os.ReadFile(filepath.Join("testdata", "codex-startup", fixture))
		if err != nil {
			t.Fatalf("reading fixture: %v", err)
		}
		pane.dialog = string(data)
		pane.trustSelected = strings.Contains(pane.dialog, "› 1. Trust and continue")
	}
	return pane
}

func (f *fakeCodexTrustPane) render() string {
	if f.quit {
		return "tester@lab:~/work$"
	}
	if f.dialog != "" {
		return strings.TrimSpace(f.dialog)
	}
	lines := []string{
		"╭──────────────────────────────────────────────╮",
		"│ >_ OpenAI Codex (v0.162.1)                   │",
		"│                                              │",
		"│ model:     gpt-5.5 high   /model to change   │",
		"│ directory: ~/work                            │",
		"╰──────────────────────────────────────────────╯",
		"",
	}
	if f.busy {
		lines = append(lines, "• Working (0s • esc to interrupt)", "")
	}
	composer := "› Ask Codex to do anything"
	if f.draft != "" {
		composer = "› " + f.draft
	}
	lines = append(lines, composer, "", "  gpt-5.5 high · ~/work")
	return strings.Join(lines, "\n")
}

func (f *fakeCodexTrustPane) execute(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if args[0] == "-L" {
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "display-message":
		if slices.Contains(args, "#{session_name}|#{session_attached}") {
			return "lab|0", nil
		}
		return "", nil
	case "show-environment":
		if slices.Contains(args, "GC_PROVIDER") {
			return "GC_PROVIDER=codex", nil
		}
		return "", errors.New("unknown variable")
	case "capture-pane":
		return f.render(), nil
	case "send-keys":
		return "", f.sendKeys(args[1:])
	default:
		return "", nil
	}
}

func (f *fakeCodexTrustPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *fakeCodexTrustPane) sendKeys(args []string) error {
	literal := false
	var keys []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t":
			i++
		case "-l":
			text, err := runtime.TmuxLiteralText(args[i+1:])
			if err != nil {
				return err
			}
			literal, keys = true, []string{text}
			i = len(args)
		default:
			keys = append(keys, args[i])
		}
	}
	if f.quit {
		return nil
	}
	if f.dialog != "" {
		f.dialogKeys = append(f.dialogKeys, keys...)
		if literal {
			return nil
		}
		for _, key := range keys {
			switch {
			case key == "Enter" && f.trustSelected:
				f.dialog = ""
				return nil
			case key == "Enter", key == "Escape":
				f.quit = true
				return nil
			}
		}
		return nil
	}
	f.keys = append(f.keys, keys...)
	if literal {
		f.draft += strings.Join(keys, " ")
		return nil
	}
	for _, key := range keys {
		switch key {
		case "C-u":
			f.draft = ""
		case "Enter":
			if f.draft != "" {
				f.submitted = append(f.submitted, f.draft)
				f.draft = ""
				f.busy = true
			}
		}
	}
	return nil
}

func (f *fakeCodexTrustPane) snapshot() (dialogKeys, keys, submitted []string, quit bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.dialogKeys), slices.Clone(f.keys), slices.Clone(f.submitted), f.quit
}

// nudgeCodexTrustPane nudges pane the way the idle-claim backstop does
// (runtime Provider.Nudge, outside session.Manager.send).
func nudgeCodexTrustPane(pane *fakeCodexTrustPane, text string) error {
	tm := NewTmux()
	tm.exec = pane
	// The backstop's wait for idle is not under test; keep it short.
	tm.cfg.NudgeIdleTimeout = 100 * time.Millisecond
	p := &Provider{tm: tm}
	return p.Nudge("lab", []runtime.ContentBlock{{Type: "text", Text: text}})
}

// The live defect: the backstop's nudge reached a Codex pane parked on its
// folder-trust dialog and typed the text, Escape and Enter into the dialog.
// The nudge answers the dialog with the trust row's keys first, then delivers
// the message to the composer.
func TestNudgeAnswersCodexFolderTrustDialogBeforeTyping(t *testing.T) {
	pane := newFakeCodexTrustPane(t, "trust-folder-0.162.1.txt")

	err := nudgeCodexTrustPane(pane, "2 beads are ready; claim one")
	dialogKeys, _, submitted, quit := pane.snapshot()
	if quit {
		t.Fatalf("Codex quit; the dialog received %q (Nudge = %v)", dialogKeys, err)
	}
	if !slices.Equal(dialogKeys, []string{"Enter"}) {
		t.Fatalf("the trust dialog received %q, want only the trust row's confirm [Enter]", dialogKeys)
	}
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if !slices.Equal(submitted, []string{"2 beads are ready; claim one"}) {
		t.Fatalf("submitted %q, want the message delivered to the composer", submitted)
	}
}

// A dialog whose cursor row cannot be located cannot be answered safely: Enter
// would confirm an unknown selection. Nothing at all is typed, and the nudge
// reports a typed error instead of a delivery.
func TestNudgeTypesNothingIntoAStartupDialogItCannotAnswer(t *testing.T) {
	prev := nudgeStartupDialogTimeout
	nudgeStartupDialogTimeout = 300 * time.Millisecond
	t.Cleanup(func() { nudgeStartupDialogTimeout = prev })
	pane := newFakeCodexTrustPane(t, "trust-folder-no-cursor-0.162.1.txt")

	err := nudgeCodexTrustPane(pane, "2 beads are ready; claim one")
	dialogKeys, keys, submitted, quit := pane.snapshot()
	if len(dialogKeys) != 0 || len(keys) != 0 || len(submitted) != 0 || quit {
		t.Fatalf("dialog keys %q, composer keys %q, submitted %q, quit %v; want nothing typed (Nudge = %v)", dialogKeys, keys, submitted, quit, err)
	}
	if !errors.Is(err, runtime.ErrStartupDialogUnanswered) {
		t.Fatalf("Nudge = %v, want runtime.ErrStartupDialogUnanswered", err)
	}
}

// Guard: a Codex pane already at its composer gets exactly the keys a nudge
// always sent it (clear, text, Escape, Enter) and nothing else.
func TestNudgeToCodexComposerSendsTheUsualKeys(t *testing.T) {
	pane := newFakeCodexTrustPane(t, "")

	if err := nudgeCodexTrustPane(pane, "run the tests"); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	_, keys, submitted, _ := pane.snapshot()
	if want := []string{"C-u", "run the tests", "Escape", "Enter"}; !slices.Equal(keys, want) {
		t.Fatalf("composer received %q, want %q", keys, want)
	}
	if !slices.Equal(submitted, []string{"run the tests"}) {
		t.Fatalf("submitted %q, want the message", submitted)
	}
}
