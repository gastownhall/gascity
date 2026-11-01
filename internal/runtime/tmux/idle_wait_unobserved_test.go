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
// each capture-pane with the next scripted capture (the last one repeats);
// every other command answers "" with an error, so the ready-prompt prefix
// falls back to its default.
type paneScriptExecutor struct {
	mu       sync.Mutex
	captures []scriptedCapture
	next     int
}

type scriptedCapture struct {
	out string
	err error
}

func (e *paneScriptExecutor) execute(args []string) (string, error) {
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
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

// ClassifyIdlePane is the one classifier both idle loops share.
func TestClassifyIdlePane(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lines    []string
		wantIdle bool
		wantErr  error
	}{
		{"busy", []string{"❯ ", busyPane}, false, nil},
		{"idle", []string{"❯ "}, true, nil},
		{"indeterminate", strings.Split(indeterminatePane, "\n"), false, runtime.ErrIdleIndeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idle, err := ClassifyIdlePane(tc.lines, DefaultReadyPromptPrefix)
			if idle != tc.wantIdle || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("ClassifyIdlePane(%q) = (%v, %v), want (%v, %v)", tc.lines, idle, err, tc.wantIdle, tc.wantErr)
			}
		})
	}
}
