package ssh

import (
	"context"
	"errors"
	"slices"
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

func TestProvider_StartQuotesNameWorkdirEnvCommand(t *testing.T) {
	// A command and env values with spaces/quotes, and a session name with a
	// space, must each be a single argv element (tmux -e takes K=V natively; the
	// command is one shell string tmux runs). ssh.shellQuote then quotes each
	// element for the remote shell, so nothing here is re-split.
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
	if err := providerWith(f).Start(context.Background(), "sess one", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := firstCall(f, isTmux("new-session"))
	want := []string{
		"tmux", "new-session", "-d", "-s", "sess one",
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
