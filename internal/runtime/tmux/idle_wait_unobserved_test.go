package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// paneScriptExecutor resolves the agent pane like fakeAgentPaneRow and answers
// each capture-pane with the next scripted capture (the last one repeats). It
// answers show-environment for a key in env with tmux's "KEY=value" line,
// trimmed the way realExecutor trims it; every other command answers "" with
// an error, so without env the ready-prompt prefix falls back to its default.
type paneScriptExecutor struct {
	mu       sync.Mutex
	captures []scriptedCapture
	next     int
	env      map[string]string
}

type scriptedCapture struct {
	out string
	err error
}

func (e *paneScriptExecutor) execute(args []string) (string, error) {
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	if tmuxSubcommand(args) == "show-environment" {
		key := args[len(args)-1]
		if value, ok := e.env[key]; ok {
			return strings.TrimSpace(key + "=" + value), nil
		}
	}
	if tmuxSubcommand(args) != "capture-pane" {
		return "", errors.New("unscripted")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.captures[min(e.next, len(e.captures)-1)]
	e.next++
	return c.out, c.err
}

func (e *paneScriptExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

func scriptedTmux(captures ...scriptedCapture) *Tmux {
	tm := NewTmux()
	tm.exec = &paneScriptExecutor{captures: captures}
	return tm
}

const (
	busyPane          = "✻ Thinking… (12s · esc to interrupt)"
	indeterminatePane = "some output\nbuilding..."
)

// A wait whose every capture failed read nothing: its timeout is no evidence
// that a turn runs (RUNTIME-CONTRACT-016).
func TestWaitForIdleWithNoReadingIsUnobserved(t *testing.T) {
	tm := scriptedTmux(scriptedCapture{err: errors.New("capture-pane: transient")})
	err := tm.WaitForIdle(context.Background(), "w", 500*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout", err)
	}
	if !errors.Is(err, runtime.ErrWaitUnobserved) {
		t.Fatalf("WaitForIdle = %v, want it to wrap ErrWaitUnobserved: no capture succeeded", err)
	}
}

// A pane showing neither its prompt nor a busy indicator (a dialog, copy mode)
// is no evidence of a running turn either.
func TestWaitForIdleIndeterminateReadingsAreUnobserved(t *testing.T) {
	tm := scriptedTmux(scriptedCapture{out: indeterminatePane})
	err := tm.WaitForIdle(context.Background(), "w", 500*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) || !errors.Is(err, runtime.ErrWaitUnobserved) {
		t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout wrapping ErrWaitUnobserved", err)
	}
}

// claudeSessionLimitMenu is Claude's session-limit chooser: the selected option
// carries the default prompt glyph, followed by an NBSP.
const claudeSessionLimitMenu = "  You've hit your session limit\n\n❯\u00a01. Stop and wait for limit to reset\n  2. Upgrade\n"

// A pane parked on a numbered menu is waiting for an answer: its cursor row is
// no idle boundary, so the wait times out without having observed a turn.
func TestWaitForIdleMenuIsNotIdle(t *testing.T) {
	tm := scriptedTmux(scriptedCapture{out: claudeSessionLimitMenu})
	err := tm.WaitForIdle(context.Background(), "w", 500*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) || !errors.Is(err, runtime.ErrWaitUnobserved) {
		t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout wrapping ErrWaitUnobserved", err)
	}
}

// The idle wait reads the session's prefix back from the tmux environment, and
// tmux output arrives trimmed, so codex's published "› " comes back as "›".
// The composer row proves the wait scanned with that prefix, not the default;
// the menu must still read as a pending choice under it.
// Kills: a menu check that needs the prefix's separator space, which reads the
// folder-trust menu under the read-back prefix as an idle composer.
func TestWaitForIdlePublishedPromptPrefixSeesMenu(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lines    []string
		wantIdle bool
	}{
		{"codex composer", []string{"› Ask Codex to do anything", "", "  gpt-5.4 high · 100% left · ~/repo"}, true},
		{"codex folder-trust menu", codexFolderTrustMenu, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &paneScriptExecutor{
				captures: []scriptedCapture{{out: strings.Join(tc.lines, "\n")}},
				env:      map[string]string{ReadyPromptEnvKey: "› "},
			}
			err := tm.WaitForIdle(context.Background(), "w", 500*time.Millisecond)
			if tc.wantIdle && err != nil {
				t.Fatalf("WaitForIdle = %v, want nil: the composer shows the published prompt", err)
			}
			if !tc.wantIdle && (!errors.Is(err, ErrIdleTimeout) || !errors.Is(err, runtime.ErrWaitUnobserved)) {
				t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout wrapping ErrWaitUnobserved: a menu cursor row is not idle", err)
			}
		})
	}
}

// The wait-idle pre-read (runtime.IdleBoundaryNow) over the tmux leaf reads a
// pane parked on a numbered menu as pending, so a wait-idle nudge queues at
// once instead of blocking its caller for a whole idle wait on a menu that
// does not close by itself. The session's published prefix "❯ " is read back
// trimmed, as the menu check sees it in production; a composer under it is
// at the boundary.
// Kills: a pre-read that reads the menu as plainly indeterminate (unknown, so
// the caller waits out runtimeHandleWaitIdleTimeout).
func TestIdleBoundaryNowMenuIsPendingChoice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		want   runtime.IdleBoundary
	}{
		{"claude composer", "● Done.\n\n❯ \n  ? for shortcuts", runtime.IdleBoundaryAt},
		{"session-limit chooser", claudeSessionLimitMenu, runtime.IdleBoundaryPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &paneScriptExecutor{
				captures: []scriptedCapture{{out: tc.screen}},
				env:      map[string]string{ReadyPromptEnvKey: DefaultReadyPromptPrefix},
			}
			if got := runtime.IdleBoundaryNow(&Provider{tm: tm}, "w"); got != tc.want {
				t.Fatalf("IdleBoundaryNow = %v, want %v", got, tc.want)
			}
		})
	}
}

// One busy reading makes the timeout an observed one, even when later reads
// fail.
func TestWaitForIdleBusyReadingIsObserved(t *testing.T) {
	tm := scriptedTmux(
		scriptedCapture{out: busyPane},
		scriptedCapture{err: errors.New("capture-pane: transient")},
	)
	err := tm.WaitForIdle(context.Background(), "w", 500*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout", err)
	}
	if errors.Is(err, runtime.ErrWaitUnobserved) {
		t.Fatalf("WaitForIdle = %v, want the bare timeout: a reading showed the session busy", err)
	}
}

// A rollout that cannot be read, or whose turn activity cannot be told, never
// showed a running turn.
func TestCodexInterruptBoundaryUnreadableRolloutIsUnobserved(t *testing.T) {
	dir := t.TempDir()
	unknown := filepath.Join(dir, "unknown.jsonl")
	// A rollout recording no lifecycle event has unknown activity.
	if err := os.WriteFile(unknown, []byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"missing rollout":  filepath.Join(dir, "missing.jsonl"),
		"unknown activity": unknown,
	} {
		t.Run(name, func(t *testing.T) {
			err := waitForCodexInterruptBoundary(context.Background(), path, time.Now(), 300*time.Millisecond)
			if !errors.Is(err, ErrIdleTimeout) || !errors.Is(err, runtime.ErrWaitUnobserved) {
				t.Fatalf("waitForCodexInterruptBoundary = %v, want ErrIdleTimeout wrapping ErrWaitUnobserved", err)
			}
		})
	}
}

// A rollout that records a running turn is an observed timeout.
func TestCodexInterruptBoundaryRunningTurnIsObserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(codexLifecycleRow("task_started", "t1")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := waitForCodexInterruptBoundary(context.Background(), path, time.Now(), 300*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("waitForCodexInterruptBoundary = %v, want ErrIdleTimeout", err)
	}
	if errors.Is(err, runtime.ErrWaitUnobserved) {
		t.Fatalf("waitForCodexInterruptBoundary = %v, want the bare timeout: the rollout recorded a running turn", err)
	}
}

// ClassifyIdlePane is the one classifier both idle loops share. A numbered
// menu's cursor row is an indeterminate reading that names the pending
// choice (runtime.ErrIdlePendingChoice); any other pane showing neither its
// prompt nor a busy indicator is plainly indeterminate.
func TestClassifyIdlePane(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lines      []string
		wantIdle   bool
		wantErr    error
		wantChoice bool
	}{
		{"busy", []string{"❯ ", busyPane}, false, nil, false},
		{"idle", []string{"❯ "}, true, nil, false},
		{"indeterminate", strings.Split(indeterminatePane, "\n"), false, runtime.ErrIdleIndeterminate, false},
		{"menu", strings.Split(claudeSessionLimitMenu, "\n"), false, runtime.ErrIdleIndeterminate, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idle, err := ClassifyIdlePane(tc.lines, DefaultReadyPromptPrefix)
			if idle != tc.wantIdle || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("ClassifyIdlePane(%q) = (%v, %v), want (%v, %v)", tc.lines, idle, err, tc.wantIdle, tc.wantErr)
			}
			if choice := errors.Is(err, runtime.ErrIdlePendingChoice); choice != tc.wantChoice {
				t.Fatalf("ClassifyIdlePane(%q) = %v: a pending choice = %t, want %t", tc.lines, err, choice, tc.wantChoice)
			}
		})
	}
}
