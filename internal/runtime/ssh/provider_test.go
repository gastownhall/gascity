package ssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// providerWith builds a Provider whose connection uses the given fake runner.
func providerWith(f *fakeRunner) *Provider {
	return &Provider{conn: &Conn{ep: Endpoint{User: "u", Host: "box"}, run: f}}
}

func firstCall(f *fakeRunner, predicate func([]string) bool) []string {
	for _, c := range f.calls {
		if predicate(c) {
			return c
		}
	}
	return nil
}

func isTmux(sub string) func([]string) bool {
	return func(argv []string) bool { return len(argv) >= 2 && argv[0] == "tmux" && argv[1] == sub }
}

func TestProvider_StartLaunchesTmuxSession(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("has-session")(argv) {
			return nil, 1, nil // not yet running
		}
		return nil, 0, nil // new-session ok
	}}
	p := providerWith(f)
	cfg := runtime.Config{Command: "agent --serve", WorkDir: "/w", Env: map[string]string{"B": "2", "A": "1"}}
	if err := p.Start(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := firstCall(f, isTmux("new-session"))
	want := []string{"tmux", "new-session", "-d", "-s", "s", "-c", "/w", "-e", "A=1", "-e", "B=2", "agent --serve"}
	if !slices.Equal(got, want) {
		t.Errorf("new-session argv =\n  %v\nwant\n  %v", got, want)
	}
}

func TestProvider_StartDuplicateIsErrSessionExists(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("has-session")(argv) {
			return nil, 0, nil // already running
		}
		return nil, 0, nil
	}}
	p := providerWith(f)
	err := p.Start(context.Background(), "s", runtime.Config{Command: "agent"})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start err = %v, want ErrSessionExists", err)
	}
	if firstCall(f, isTmux("new-session")) != nil {
		t.Error("new-session must not be issued when the session already exists")
	}
}

func TestProvider_StopIsIdempotent(t *testing.T) {
	// kill-session on a missing session exits non-zero; Stop must still return nil.
	f := &fakeRunner{code: 1}
	p := providerWith(f)
	if err := p.Stop("s"); err != nil {
		t.Fatalf("Stop should be idempotent, got %v", err)
	}
	if firstCall(f, isTmux("kill-session")) == nil {
		t.Error("Stop should issue tmux kill-session")
	}
}

func TestProvider_IsRunning(t *testing.T) {
	running := &fakeRunner{code: 0}
	if !providerWith(running).IsRunning("s") {
		t.Error("IsRunning = false when has-session exits 0")
	}
	missing := &fakeRunner{code: 1}
	if providerWith(missing).IsRunning("s") {
		t.Error("IsRunning = true when has-session exits 1")
	}
}

func TestProvider_NudgeDrivesNamedTmuxTarget(t *testing.T) {
	// The carrier target is the session name (one host, many sessions).
	f := &fakeRunner{}
	p := providerWith(f)
	if err := p.Nudge("sess-7", runtime.TextContent("hi")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	want := [][]string{
		{"tmux", "send-keys", "-t", "sess-7", "-l", "hi"},
		{"tmux", "send-keys", "-t", "sess-7", "Enter"},
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %v, want 2", f.calls)
	}
	for i := range want {
		if !slices.Equal(f.calls[i], want[i]) {
			t.Errorf("call[%d] = %v, want %v", i, f.calls[i], want[i])
		}
	}
}

func TestProvider_ListRunningFiltersByPrefix(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("list-sessions")(argv) {
			return []byte("sess-1\nsess-2\nother\n"), 0, nil
		}
		return nil, 0, nil
	}}
	got, err := providerWith(f).ListRunning("sess-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if !slices.Equal(got, []string{"sess-1", "sess-2"}) {
		t.Errorf("ListRunning = %v, want [sess-1 sess-2]", got)
	}
}

func TestProvider_GetMeta(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("show-environment")(argv) {
			return []byte("KEY=the value\n"), 0, nil
		}
		return nil, 0, nil
	}}
	val, err := providerWith(f).GetMeta("s", "KEY")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "the value" {
		t.Errorf("GetMeta = %q, want %q", val, "the value")
	}
}

func TestProvider_ProcessAliveEmptyIsTrue(t *testing.T) {
	if !providerWith(&fakeRunner{}).ProcessAlive("s", nil) {
		t.Error("ProcessAlive with no names should be true")
	}
}

func TestProvider_StartRejectsUnsafeName(t *testing.T) {
	// A name with tmux target metacharacters (".", ":") or empty must be rejected
	// before any tmux op, since the carrier addresses the session by name.
	f := &fakeRunner{}
	p := providerWith(f)
	for _, bad := range []string{"a.b", "a:b", ""} {
		if err := p.Start(context.Background(), bad, runtime.Config{Command: "x"}); !errors.Is(err, ErrInvalidSessionName) {
			t.Errorf("Start(%q) err = %v, want ErrInvalidSessionName", bad, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("no tmux ops should run for a rejected name; got %v", f.calls)
	}
}

func TestProvider_StartQuotesNameWorkdirEnvCommand(t *testing.T) {
	// Command and env values with spaces/quotes must each be a single argv element
	// (tmux -e takes K=V natively; the command is one shell string tmux runs).
	// ssh.shellQuote then quotes each element for the remote shell, so nothing
	// here is re-split. (The session name itself is restricted to a safe tmux
	// target, so it carries no spaces — see TestProvider_StartRejectsUnsafeName.)
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("has-session")(argv) {
			return nil, 1, nil // not running
		}
		return nil, 0, nil
	}}
	cfg := runtime.Config{
		Command: `agent --flag "a b"`,
		WorkDir: "/path with space",
		Env:     map[string]string{"MSG": "hello world", "Q": `a'b"c`},
	}
	if err := providerWith(f).Start(context.Background(), "sess-one", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := firstCall(f, isTmux("new-session"))
	want := []string{
		"tmux", "new-session", "-d", "-s", "sess-one",
		"-c", "/path with space",
		"-e", "MSG=hello world",
		"-e", `Q=a'b"c`,
		`agent --flag "a b"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("new-session argv =\n  %#v\nwant\n  %#v", got, want)
	}
}

func TestProvider_StartTransportFailureIsNotDuplicate(t *testing.T) {
	// If the box is unreachable, the has-session precheck reads not-running and
	// new-session then transport-fails: Start must error, never ErrSessionExists.
	f := &fakeRunner{respond: func([]string) ([]byte, int, error) {
		return nil, -1, context.DeadlineExceeded
	}}
	err := providerWith(f).Start(context.Background(), "s", runtime.Config{Command: "x"})
	if err == nil {
		t.Fatal("Start on an unreachable box must error")
	}
	if errors.Is(err, runtime.ErrSessionExists) {
		t.Errorf("transport failure must not be reported as ErrSessionExists: %v", err)
	}
}

func TestProvider_ProcessAliveBracketsPattern(t *testing.T) {
	// The pgrep pattern brackets its first character so it cannot self-match
	// the wrapping shell's own argv over ssh (the dash false-positive).
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if len(argv) >= 1 && argv[0] == "pgrep" {
			return nil, 0, nil // found
		}
		return nil, 1, nil
	}}
	p := providerWith(f)
	if !p.ProcessAlive("s", []string{"claude"}) {
		t.Error("ProcessAlive should be true when pgrep matches")
	}
	got := firstCall(f, func(a []string) bool { return len(a) >= 1 && a[0] == "pgrep" })
	want := []string{"pgrep", "-f", "[c]laude"}
	if !slices.Equal(got, want) {
		t.Errorf("pgrep argv = %v, want %v (first char must be bracketed)", got, want)
	}
}

func TestProvider_ProcessAliveAbsentIsFalse(t *testing.T) {
	f := &fakeRunner{code: 1} // pgrep finds nothing
	if providerWith(f).ProcessAlive("s", []string{"ghost"}) {
		t.Error("ProcessAlive should be false when pgrep matches nothing")
	}
}

func TestProvider_StartRunsPreStartAndAbortsOnFailure(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			return nil, 1, nil // not running
		case len(argv) >= 2 && argv[0] == "sh" && argv[1] == "-c":
			return []byte("boom"), 3, nil // a PreStart command fails
		}
		return nil, 0, nil
	}}
	err := providerWith(f).Start(context.Background(), "s", runtime.Config{Command: "agent", PreStart: []string{"mkdir /x"}})
	if err == nil {
		t.Fatal("Start must abort when a PreStart command fails")
	}
	if firstCall(f, isTmux("new-session")) != nil {
		t.Error("new-session must not run after a PreStart failure")
	}
}

func TestProvider_StartRunsSessionSetupOnBox(t *testing.T) {
	created := false
	var setup [][]string
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			if created {
				return nil, 0, nil // alive on the liveness recheck
			}
			return nil, 1, nil // precheck: not yet running
		case isTmux("new-session")(argv):
			created = true
		case len(argv) >= 2 && argv[0] == "sh" && argv[1] == "-c":
			setup = append(setup, argv)
		}
		return nil, 0, nil
	}}
	cfg := runtime.Config{Command: "agent", SessionSetup: []string{"echo hi", "touch x"}}
	if err := providerWith(f).Start(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(setup) != 2 {
		t.Fatalf("session_setup calls = %v, want 2", setup)
	}
	// Each command runs via `sh -c` with the env prelude (GC_SESSION) prepended.
	for i, want := range []string{"echo hi", "touch x"} {
		arg := setup[i][2]
		if !strings.HasSuffix(arg, want) {
			t.Errorf("session_setup[%d] = %q, want command suffix %q", i, arg, want)
		}
		if !strings.Contains(arg, "export GC_SESSION='s'") {
			t.Errorf("session_setup[%d] missing GC_SESSION export: %q", i, arg)
		}
	}
}

func TestProvider_StartSetupCarriesWorkdirAndEnv(t *testing.T) {
	created := false
	var pre []string
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			if created {
				return nil, 0, nil
			}
			return nil, 1, nil
		case isTmux("new-session")(argv):
			created = true
		case len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c":
			pre = argv
		}
		return nil, 0, nil
	}}
	cfg := runtime.Config{Command: "agent", WorkDir: "/w space", Env: map[string]string{"FOO": "bar baz"}, PreStart: []string{"prep"}}
	if err := providerWith(f).Start(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if pre == nil {
		t.Fatal("PreStart sh -c was not issued")
	}
	for _, want := range []string{`cd '/w space' || exit 1`, `export FOO='bar baz'`, `export GC_SESSION='s'`, "prep"} {
		if !strings.Contains(pre[2], want) {
			t.Errorf("PreStart sh -c arg missing %q:\n%s", want, pre[2])
		}
	}
}

func TestProvider_StartRunsSessionLiveAtStartup(t *testing.T) {
	created := false
	var live [][]string
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			if created {
				return nil, 0, nil
			}
			return nil, 1, nil
		case isTmux("new-session")(argv):
			created = true
		case len(argv) >= 2 && argv[0] == "sh" && argv[1] == "-c":
			live = append(live, argv)
		}
		return nil, 0, nil
	}}
	cfg := runtime.Config{Command: "agent", SessionLive: []string{"tmux-theme"}}
	if err := providerWith(f).Start(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(live) != 1 || !strings.HasSuffix(live[0][2], "tmux-theme") {
		t.Errorf("session_live not applied at startup: %v", live)
	}
}

func TestProvider_StartShipsSessionSetupScriptViaStdin(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "setup.sh")
	const body = "#!/bin/sh\necho configured\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	created := false
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			if created {
				return nil, 0, nil // alive on the liveness recheck
			}
			return nil, 1, nil // precheck: not yet running
		case isTmux("new-session")(argv):
			created = true
		}
		return nil, 0, nil
	}}
	if err := providerWith(f).Start(context.Background(), "s", runtime.Config{Command: "agent", SessionSetupScript: scriptPath}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The script ships via a remote `sh` with its content on stdin.
	idx := -1
	for i, c := range f.calls {
		if len(c) == 1 && c[0] == "sh" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no `sh` (script) call recorded; calls=%v", f.calls)
	}
	got := string(f.stdins[idx])
	if !strings.Contains(got, body) {
		t.Errorf("script stdin = %q, want it to contain the file content", got)
	}
	if !strings.Contains(got, "export GC_SESSION='s'") {
		t.Errorf("script stdin missing the env prelude (GC_SESSION): %q", got)
	}
}

func TestProvider_StartPostLivenessDetectsImmediateDeath(t *testing.T) {
	// A managed-hints (Nudge) session whose tmux session is gone on the
	// liveness recheck yields ErrSessionDiedDuringStartup.
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("has-session")(argv) {
			return nil, 1, nil // never present: precheck (proceed) AND liveness (died)
		}
		return nil, 0, nil // new-session "succeeds"
	}}
	p := providerWith(f) // postStartSettle == 0, no sleep
	err := p.Start(context.Background(), "s", runtime.Config{Command: "agent", Nudge: "go"})
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Start err = %v, want ErrSessionDiedDuringStartup", err)
	}
}

func TestProvider_StartSendsInitialNudgeWhenAlive(t *testing.T) {
	created := false
	var nudges [][]string
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			if created {
				return nil, 0, nil // alive on the liveness recheck
			}
			return nil, 1, nil // precheck: not yet running
		case isTmux("new-session")(argv):
			created = true
			return nil, 0, nil
		case isTmux("send-keys")(argv):
			nudges = append(nudges, argv)
			return nil, 0, nil
		}
		return nil, 0, nil
	}}
	if err := providerWith(f).Start(context.Background(), "s", runtime.Config{Command: "agent", Nudge: "hello"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(nudges) != 2 {
		t.Fatalf("expected 2 send-keys (type + Enter) for the initial nudge, got %v", nudges)
	}
	if !slices.Equal(nudges[0], []string{"tmux", "send-keys", "-t", "s", "-l", "hello"}) {
		t.Errorf("initial nudge type = %v", nudges[0])
	}
}

func TestProvider_AttachArgsQuotesRemoteCommand(t *testing.T) {
	// A session name with shell metacharacters must be confined to a single
	// shell-quoted remote-command argument — no remote command injection.
	args := attachArgs(Endpoint{User: "u", Host: "box"}, "x; rm -rf ~")
	if last := args[len(args)-1]; last != `'tmux' 'attach' '-t' 'x; rm -rf ~'` {
		t.Errorf("remote command arg = %q, want it shell-quoted as one token", last)
	}
	if dest := args[len(args)-2]; dest != "u@box" {
		t.Errorf("destination = %q, want u@box", dest)
	}
	if slices.Contains(args, "BatchMode=yes") {
		t.Error("attach must not set BatchMode=yes (operator may need to answer a prompt)")
	}
	if !slices.Contains(args, "-t") {
		t.Error("attach must force a PTY with -t")
	}
}
