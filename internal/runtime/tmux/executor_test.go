package tmux

import (
	"context"
	"testing"
)

// fakeExecutor captures tmux command arguments for unit testing.
type fakeExecutor struct {
	calls [][]string // each call's full args
	out   string
	err   error
	outs  []string
	errs  []error
	idx   int
}

func (f *fakeExecutor) execute(args []string) (string, error) {
	// Copy args to avoid aliasing with the caller's slice.
	cp := make([]string, len(args))
	copy(cp, args)
	f.calls = append(f.calls, cp)
	if f.idx < len(f.outs) || f.idx < len(f.errs) {
		var out string
		var err error
		if f.idx < len(f.outs) {
			out = f.outs[f.idx]
		}
		if f.idx < len(f.errs) {
			err = f.errs[f.idx]
		}
		f.idx++
		return out, err
	}
	return f.out, f.err
}

func (f *fakeExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func TestRunInjectsSocketFlag(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "bright-lights"}, exec: fe}
	_, _ = tm.run("list-sessions")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	want := []string{"-u", "-L", "bright-lights", "list-sessions"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunNoSocketFlagWhenEmpty(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	_, _ = tm.run("list-sessions")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	want := []string{"-u", "list-sessions"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunAlwaysPrependsUTF8Flag(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	_, _ = tm.run("new-session", "-s", "test")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	if got[0] != "-u" {
		t.Errorf("args[0] = %q, want %q", got[0], "-u")
	}
	// Verify full arg list: -u -L x new-session -s test
	want := []string{"-u", "-L", "x", "new-session", "-s", "test"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsSessionRunningFalseWhenPaneDead(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"", "1"},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	if tm.IsSessionRunning("runner") {
		t.Fatal("IsSessionRunning = true, want false for dead pane")
	}

	if len(fe.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(fe.calls))
	}
	want := [][]string{
		{"-u", "-L", "x", "has-session", "-t", "=runner"},
		{"-u", "-L", "x", "display-message", "-t", "runner:^.0", "-p", "#{pane_dead}"},
	}
	for i := range want {
		if len(fe.calls[i]) != len(want[i]) {
			t.Fatalf("call %d = %v, want %v", i, fe.calls[i], want[i])
		}
		for j := range want[i] {
			if fe.calls[i][j] != want[i][j] {
				t.Errorf("call %d arg %d = %q, want %q", i, j, fe.calls[i][j], want[i][j])
			}
		}
	}
}

func TestIsSessionRunningFallsBackToSessionExistsOnPaneQueryError(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{""},
		errs: []error{nil, ErrNoServer},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	if !tm.IsSessionRunning("runner") {
		t.Fatal("IsSessionRunning = false, want true when pane query fails after session exists")
	}
}
