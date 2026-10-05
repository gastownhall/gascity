package tmux

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// tmux resolves a bare "-t name" by unique prefix: once "worker-1" has exited,
// "-t worker-1" names a live "worker-10". These tests pin the exact target
// every session-keyed destructive, meta, fence and input command sends.

// exactSessionCommands take a target-session; exactPaneCommands take a
// target-window or target-pane, where only "=name:" is exact.
var (
	exactSessionCommands = []string{"kill-session", "set-environment", "show-environment", "rename-session"}
	exactPaneCommands    = []string{"display-message", "send-keys", "paste-buffer", "capture-pane", "list-panes", "respawn-pane", "clear-history"}
)

// scriptedTargetExecutor records argv and answers capture-pane from captures
// (then capture), display-message with display, and everything else with "".
type scriptedTargetExecutor struct {
	calls    [][]string
	captures []string
	capture  string
	display  string
}

func (s *scriptedTargetExecutor) execute(args []string) (string, error) {
	s.calls = append(s.calls, slices.Clone(args))
	switch {
	case slices.Contains(args, "capture-pane"):
		if len(s.captures) > 0 {
			out := s.captures[0]
			s.captures = s.captures[1:]
			return out, nil
		}
		return s.capture, nil
	case slices.Contains(args, "display-message"):
		return s.display, nil
	}
	return "", nil
}

func (s *scriptedTargetExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return s.execute(args)
}

// tmuxSubcommand returns the subcommand after runCtx's "-u" and "-L sock".
func tmuxSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-u":
		case "-L":
			i++
		default:
			return args[i]
		}
	}
	return ""
}

func tmuxTargetArg(args []string) (string, bool) {
	i := slices.Index(args, "-t")
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

// requireExactTargets fails on any session- or pane-kind command whose target
// is not the exact form of name, and on any wanted subcommand that never ran.
func requireExactTargets(t *testing.T, calls [][]string, name string, want ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, call := range calls {
		sub := tmuxSubcommand(call)
		target, ok := tmuxTargetArg(call)
		if !ok {
			continue
		}
		switch {
		case slices.Contains(exactSessionCommands, sub):
			if target != "="+name {
				t.Errorf("%s -t %q, want %q; argv=%q", sub, target, "="+name, call)
			}
		case slices.Contains(exactPaneCommands, sub):
			if target != "="+name+":" && target != "="+name+":^.0" {
				t.Errorf("%s -t %q, want %q or %q; argv=%q", sub, target, "="+name+":", "="+name+":^.0", call)
			}
		default:
			continue
		}
		seen[sub] = true
	}
	for _, sub := range want {
		if !seen[sub] {
			t.Errorf("no targeted %s call; calls=%q", sub, calls)
		}
	}
}

func notFoundErr(args ...string) error {
	return wrapError(errors.New("exit status 1"), "can't find session: worker-1", args)
}

func TestKillSessionTargetsExactSession(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{exec: fe}

	if err := tm.KillSession("worker-1"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	want := []string{"-u", "kill-session", "-t", "=worker-1"}
	if len(fe.calls) != 1 || !slices.Equal(fe.calls[0], want) {
		t.Fatalf("calls = %q, want [%q]", fe.calls, want)
	}
}

// The tmux 3.4 answers for a vanished worker-1 beside a live worker-10: the
// exact kill-plan probe expands to nothing and the exact kill-session cannot
// find the session. Stop stays idempotent and never names a bare target.
func TestStopOnVanishedSessionNeverTargetsByPrefix(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"", ""},
		errs: []error{nil, notFoundErr("kill-session")},
	}
	p := &Provider{tm: &Tmux{exec: fe}, cache: NewStateCache(nil, time.Hour)}

	if err := p.Stop("worker-1"); err != nil {
		t.Fatalf("Stop on a vanished session = %v, want nil", err)
	}
	want := [][]string{
		{"-u", "display-message", "-t", "=worker-1:^.0", "-p", "#{pane_pid}\t#{pane_dead}"},
		{"-u", "kill-session", "-t", "=worker-1"},
	}
	if len(fe.calls) != len(want) {
		t.Fatalf("calls = %q, want %q", fe.calls, want)
	}
	for i := range want {
		if !slices.Equal(fe.calls[i], want[i]) {
			t.Errorf("call %d = %q, want %q", i, fe.calls[i], want[i])
		}
	}
}

func TestPaneTargetKeepsPaneIDsAndQualifiedTargets(t *testing.T) {
	cases := []struct {
		in, session, pane, primary string
	}{
		{"worker-1", "=worker-1", "=worker-1:", "=worker-1:^.0"},
		{"%12", "%12", "%12", "%12"},
		{"s:1.0", "s:1.0", "s:1.0", "s:1.0:^.0"},
		{"=worker-1", "=worker-1", "=worker-1", "=worker-1:^.0"},
	}
	for _, tc := range cases {
		if got := sessionTarget(tc.in); got != tc.session {
			t.Errorf("sessionTarget(%q) = %q, want %q", tc.in, got, tc.session)
		}
		if got := paneTarget(tc.in); got != tc.pane {
			t.Errorf("paneTarget(%q) = %q, want %q", tc.in, got, tc.pane)
		}
		if got := primaryPaneTarget(tc.in); got != tc.primary {
			t.Errorf("primaryPaneTarget(%q) = %q, want %q", tc.in, got, tc.primary)
		}
	}
}

// "list-panes -s -t =name" still prefix-matches on tmux 3.4; only "=name:"
// is exact.
func TestSessionPanesDeadUsesPaneScopedExactTarget(t *testing.T) {
	fe := &fakeExecutor{out: "1"}
	tm := &Tmux{exec: fe}

	if _, err := tm.sessionPanesDead("worker-1"); err != nil {
		t.Fatalf("sessionPanesDead: %v", err)
	}
	want := []string{"-u", "list-panes", "-s", "-t", "=worker-1:", "-F", "#{pane_dead}"}
	if len(fe.calls) != 1 || !slices.Equal(fe.calls[0], want) {
		t.Fatalf("calls = %q, want [%q]", fe.calls, want)
	}
}

func TestEnvironmentOpsTargetExactSession(t *testing.T) {
	cases := []struct {
		name string
		out  string
		run  func(*Tmux) error
		want []string
	}{
		{
			"SetEnvironment", "", func(tm *Tmux) error { return tm.SetEnvironment("worker-1", "GC_SESSION_ID", "gc-1") },
			[]string{"-u", "set-environment", "-t", "=worker-1", "GC_SESSION_ID", "gc-1"},
		},
		{
			"RemoveEnvironment", "", func(tm *Tmux) error { return tm.RemoveEnvironment("worker-1", "GC_K") },
			[]string{"-u", "set-environment", "-t", "=worker-1", "-u", "GC_K"},
		},
		{
			"GetEnvironment", "GC_K=v", func(tm *Tmux) error { _, err := tm.GetEnvironment("worker-1", "GC_K"); return err },
			[]string{"-u", "show-environment", "-t", "=worker-1", "GC_K"},
		},
		{
			"GetAllEnvironment", "GC_K=v", func(tm *Tmux) error { _, err := tm.GetAllEnvironment("worker-1"); return err },
			[]string{"-u", "show-environment", "-t", "=worker-1"},
		},
		{
			"markSessionEnvRemoved", "", func(tm *Tmux) error { return tm.markSessionEnvRemoved("worker-1", []string{"GC_K"}) },
			[]string{"-u", "set-environment", "-t", "=worker-1", "-r", "GC_K"},
		},
		{
			"RenameSession", "", func(tm *Tmux) error { return tm.RenameSession("worker-1", "worker-2") },
			[]string{"-u", "rename-session", "-t", "=worker-1", "worker-2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{out: tc.out}
			if err := tc.run(&Tmux{exec: fe}); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(fe.calls) != 1 || !slices.Equal(fe.calls[0], tc.want) {
				t.Fatalf("calls = %q, want [%q]", fe.calls, tc.want)
			}
		})
	}
}

// Input typed or pasted into a prefix-matched session lands in another agent.
func TestInputOpsTargetExactPane(t *testing.T) {
	cases := []struct {
		name     string
		ex       scriptedTargetExecutor
		run      func(*Tmux)
		wantSubs []string
	}{
		{
			"SendKeysDebounced",
			scriptedTargetExecutor{display: "1"},
			func(tm *Tmux) { _ = tm.SendKeysDebounced("worker-1", "hi", 0) },
			[]string{"display-message", "send-keys"},
		},
		{
			"SendKeysRaw",
			scriptedTargetExecutor{},
			func(tm *Tmux) { _ = tm.SendKeysRaw("worker-1", "Enter") },
			[]string{"send-keys"},
		},
		{
			"SendKeysReplace",
			scriptedTargetExecutor{},
			func(tm *Tmux) { _ = tm.SendKeysReplace("worker-1", "hi", 0) },
			[]string{"send-keys"},
		},
		{
			"sendLiteralText",
			scriptedTargetExecutor{},
			func(tm *Tmux) { _ = tm.sendLiteralText("worker-1", "hi") },
			[]string{"send-keys"},
		},
		{
			"pasteLiteralText",
			scriptedTargetExecutor{},
			func(tm *Tmux) { _ = tm.pasteLiteralText("worker-1", "hi") },
			[]string{"paste-buffer"},
		},
		{
			"sendNudgeSubmitSequence",
			scriptedTargetExecutor{},
			func(tm *Tmux) { _ = tm.sendNudgeSubmitSequence("worker-1", []string{"Enter"}) },
			[]string{"send-keys"},
		},
		{
			"NudgeSession",
			scriptedTargetExecutor{},
			func(tm *Tmux) { _ = tm.NudgeSession("worker-1", "hi") },
			[]string{"display-message", "send-keys", "capture-pane", "list-panes", "show-environment"},
		},
		{
			"NudgePane",
			scriptedTargetExecutor{},
			func(tm *Tmux) { _ = tm.NudgePane("worker-1", "hi") },
			[]string{"send-keys"},
		},
		{
			"DismissModelSwitchModalIfPresent",
			scriptedTargetExecutor{captures: []string{"Approaching rate limits\n" +
				"Switch to gpt-5.4-mini for lower credit usage?\n  2. Keep current model\nPress enter to confirm or esc to go back"}},
			func(tm *Tmux) { tm.DismissModelSwitchModalIfPresent("worker-1") },
			[]string{"capture-pane", "send-keys"},
		},
		{
			"DismissFeedbackSurveyModalIfPresent",
			scriptedTargetExecutor{captures: []string{feedbackSurveySessionFixture, feedbackSurveySessionFixture}, display: "worker-1|0"},
			func(tm *Tmux) { _ = tm.DismissFeedbackSurveyModalIfPresent("worker-1") },
			[]string{"capture-pane", "display-message", "send-keys"},
		},
		{
			"DismissKnownDialogs",
			scriptedTargetExecutor{capture: "Resume from summary\nResume full session as-is\nEnter to confirm"},
			func(tm *Tmux) { _ = tm.DismissKnownDialogs(context.Background(), "worker-1", 10*time.Millisecond) },
			[]string{"capture-pane", "send-keys"},
		},
		{
			"CapturePaneJoined",
			scriptedTargetExecutor{},
			func(tm *Tmux) { _, _ = tm.CapturePaneJoined("worker-1", 5) },
			[]string{"capture-pane"},
		},
		{
			"CapturePaneAll",
			scriptedTargetExecutor{},
			func(tm *Tmux) { _, _ = tm.CapturePaneAll("worker-1") },
			[]string{"capture-pane"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := tc.ex
			cfg := DefaultConfig()
			cfg.DebounceMs = 0
			tc.run(&Tmux{cfg: cfg, exec: &ex})
			requireExactTargets(t, ex.calls, "worker-1", tc.wantSubs...)
		})
	}
}

// Pane reads feed liveness, the kill plan and respawn. Answered for a
// prefix-matched session they describe, or replace, the wrong process.
func TestPaneReadsAndPaneOpsTargetExactSession(t *testing.T) {
	cases := []struct {
		name    string
		display string
		run     func(*Tmux)
		wantSub string
	}{
		{"GetPaneCommand", "claude", func(tm *Tmux) { _, _ = tm.GetPaneCommand("worker-1") }, "display-message"},
		{"GetPaneID", "%1", func(tm *Tmux) { _, _ = tm.GetPaneID("worker-1") }, "display-message"},
		{"GetPaneWorkDir", "/w", func(tm *Tmux) { _, _ = tm.GetPaneWorkDir("worker-1") }, "display-message"},
		{"GetPanePID", "42", func(tm *Tmux) { _, _ = tm.GetPanePID("worker-1") }, "display-message"},
		{"IsPaneDead", "0", func(tm *Tmux) { _, _ = tm.IsPaneDead("worker-1") }, "display-message"},
		{"PaneDeadInfo", "0|", func(tm *Tmux) { _, _ = tm.PaneDeadInfo("worker-1") }, "display-message"},
		{"IsSessionAttached", "1", func(tm *Tmux) { _ = tm.IsSessionAttached("worker-1") }, "display-message"},
		{"GetSessionCreatedUnix", "1", func(tm *Tmux) { _, _ = tm.GetSessionCreatedUnix("worker-1") }, "display-message"},
		{"FindAgentPane", "", func(tm *Tmux) { _, _ = tm.FindAgentPane("worker-1") }, "list-panes"},
		{"CapturePane", "", func(tm *Tmux) { _, _ = tm.CapturePane("worker-1", 5) }, "capture-pane"},
		{"RespawnPane", "", func(tm *Tmux) { _ = tm.RespawnPane("worker-1", "true") }, "respawn-pane"},
		{"RespawnPaneWithWorkDir", "", func(tm *Tmux) { _ = tm.RespawnPaneWithWorkDir("worker-1", "/w", "true") }, "respawn-pane"},
		{"ClearHistory", "", func(tm *Tmux) { _ = tm.ClearHistory("worker-1") }, "clear-history"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := &scriptedTargetExecutor{display: tc.display}
			tc.run(&Tmux{cfg: DefaultConfig(), exec: ex})
			requireExactTargets(t, ex.calls, "worker-1", tc.wantSub)
		})
	}
}

func TestWrapErrorMapsNoSuchSession(t *testing.T) {
	err := wrapError(errors.New("exit status 1"), "no such session: =worker-1", []string{"show-environment"})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("wrapError(no such session) = %v, want ErrSessionNotFound", err)
	}
}

func TestGetMetaUnsetKeyIsEmpty(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
	}{
		{"unknown variable", "", wrapError(errors.New("exit status 1"), "unknown variable: GC_K", []string{"show-environment"})},
		{"marked for removal", "-GC_K", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{tm: &Tmux{exec: &fakeExecutor{out: tc.out, err: tc.err}}}
			got, err := p.GetMeta("worker-1", "GC_K")
			if err != nil || got != "" {
				t.Fatalf("GetMeta = (%q, %v), want (\"\", nil)", got, err)
			}
		})
	}
}

func TestGetMetaFailuresAreTyped(t *testing.T) {
	failed := func(stderr string) error {
		return wrapError(errors.New("exit status 1"), stderr, []string{"show-environment"})
	}
	cases := []struct {
		name     string
		out      string
		err      error
		wantIs   []error
		wantNots []error
	}{
		{
			"no such session", "", failed("no such session: =worker-1"),
			[]error{runtime.ErrSessionNotFound, ErrSessionNotFound},
			[]error{runtime.ErrRuntimeUnavailable},
		},
		{
			"can't find session", "", failed("can't find session: worker-1"),
			[]error{runtime.ErrSessionNotFound},
			[]error{runtime.ErrRuntimeUnavailable},
		},
		{
			"no server", "", failed("no server running on /tmp/tmux-1000/x"),
			[]error{runtime.ErrRuntimeUnavailable, ErrNoServer},
			[]error{runtime.ErrSessionNotFound},
		},
		{
			"timeout", "", context.DeadlineExceeded,
			[]error{runtime.ErrRuntimeUnavailable},
			[]error{runtime.ErrSessionNotFound},
		},
		{
			"unparsable", "garbage", nil,
			[]error{runtime.ErrRuntimeUnavailable},
			[]error{runtime.ErrSessionNotFound},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{tm: &Tmux{exec: &fakeExecutor{out: tc.out, err: tc.err}}}
			got, err := p.GetMeta("worker-1", "GC_K")
			if err == nil || got != "" {
				t.Fatalf("GetMeta = (%q, %v), want (\"\", error)", got, err)
			}
			for _, want := range tc.wantIs {
				if !errors.Is(err, want) {
					t.Errorf("GetMeta error %v does not wrap %v", err, want)
				}
			}
			for _, not := range tc.wantNots {
				if errors.Is(err, not) {
					t.Errorf("GetMeta error %v wraps %v", err, not)
				}
			}
			if isNoServerError(err) != errors.Is(tc.err, ErrNoServer) {
				t.Errorf("isNoServerError(%v) = %t", err, isNoServerError(err))
			}
		})
	}
}
