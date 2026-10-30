package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
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

// isStagingScript matches the secret-env staging call: execScript runs a bare
// `sh` and feeds the script on stdin.
func isStagingScript(argv []string) bool { return len(argv) == 1 && argv[0] == "sh" }

// stagedDir is the remote directory the fake staging script reports creating.
const stagedDir = "/tmp/gc-session-test"

// answerStaging makes a fake runner reply to the staging script with a
// directory (as the real script does, on stdout), deferring everything else to
// next. Any test whose Config carries a secret env value needs this.
func answerStaging(next func([]string) ([]byte, int, error)) func([]string) ([]byte, int, error) {
	return func(argv []string) ([]byte, int, error) {
		if isStagingScript(argv) {
			return []byte(stagedDir + "\n"), 0, nil
		}
		if next == nil {
			return nil, 0, nil
		}
		return next(argv)
	}
}

// stdinFor returns the stdin the fake runner saw for the first call matching
// predicate.
func stdinFor(f *fakeRunner, predicate func([]string) bool) string {
	for i, c := range f.calls {
		if predicate(c) {
			return string(f.stdins[i])
		}
	}
	return ""
}

func TestProvider_StartLaunchesTmuxSession(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("has-session")(argv) {
			return nil, 1, nil // not yet running
		}
		return nil, 0, nil // new-session ok
	}}
	p := providerWith(f)
	// Inert env only (see runtime.ArgvSafeEnvKey): values that cannot
	// authenticate anything still ride -e, so no temp file is needed on a box
	// whose sessions carry no credentials.
	cfg := runtime.Config{Command: "agent --serve", WorkDir: "/w", Env: map[string]string{"GC_RIG": "2", "GC_AGENT": "1"}}
	if err := p.Start(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := firstCall(f, isTmux("new-session"))
	want := []string{"tmux", "new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "s", "-c", "/w", "-e", "GC_AGENT=1", "-e", "GC_RIG=2", "agent --serve"}
	if !slices.Equal(got, want) {
		t.Errorf("new-session argv =\n  %v\nwant\n  %v", got, want)
	}
	if firstCall(f, isStagingScript) != nil {
		t.Error("an all-inert environment must not stage a file on the box")
	}
}

// TestProvider_StartKeepsSecretEnvOutOfArgv is the guard for the ssh provider:
// a credential must not reach any command line, on either end of the
// connection. The new-session command moves into a staged tmux command file and
// only its path is named.
func TestProvider_StartKeepsSecretEnvOutOfArgv(t *testing.T) {
	const secret = "sk-test-not-a-real-credential"
	created := false
	f := &fakeRunner{respond: answerStaging(func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			if created {
				return nil, 0, nil
			}
			return nil, 1, nil
		case isTmux("start-server")(argv):
			created = true
		}
		return nil, 0, nil
	})}
	cfg := runtime.Config{
		Command:  "agent",
		WorkDir:  "/w",
		Env:      map[string]string{"ANTHROPIC_AUTH_TOKEN": secret, "GC_RIG": "r"},
		PreStart: []string{"prep"},
	}
	if err := providerWith(f).Start(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for _, c := range f.calls {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Fatalf("secret value reached remote argv: %v", c)
			}
		}
	}
	if firstCall(f, isTmux("new-session")) != nil {
		t.Error("new-session must not be issued as argv when the env holds a secret")
	}
	got := firstCall(f, isTmux("start-server"))
	want := []string{"tmux", "start-server", ";", "source-file", stagedDir + "/session.tmux"}
	if !slices.Equal(got, want) {
		t.Errorf("launch argv =\n  %v\nwant\n  %v", got, want)
	}

	// The values travel on stdin instead, in both staged files.
	script := stdinFor(f, isStagingScript)
	if !strings.Contains(script, "ANTHROPIC_AUTH_TOKEN="+shellQuote([]string{secret})) {
		t.Error("staged sh env file does not export the secret")
	}
	if !strings.Contains(script, tmuxQuote("ANTHROPIC_AUTH_TOKEN="+secret)) {
		t.Error("staged tmux command file does not set the secret in the session env")
	}
	if !strings.Contains(script, "umask 077") {
		t.Error("staging script does not restrict the umask")
	}
	if !strings.Contains(script, `chmod 600 "$d"/*`) {
		t.Error("staging script does not chmod the staged files")
	}

	// GC_RIG is inert, so it stays inline in the prelude rather than the file.
	pre := firstCall(f, func(argv []string) bool {
		return len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" && strings.HasSuffix(argv[2], "prep")
	})
	if pre == nil {
		t.Fatal("PreStart sh -c was not issued")
	}
	if !strings.Contains(pre[2], ". '"+stagedDir+"/env.sh'") {
		t.Errorf("prelude does not source the staged env file:\n%s", pre[2])
	}
	if !strings.Contains(pre[2], "export GC_RIG='r'") {
		t.Errorf("prelude dropped the inert env var:\n%s", pre[2])
	}

	// The staged directory is removed once the session is up.
	if firstCall(f, func(argv []string) bool {
		return slices.Equal(argv, []string{"rm", "-rf", stagedDir})
	}) == nil {
		t.Error("staged directory was not cleaned up")
	}
}

// TestProvider_StartFailsClosedWhenStagingFails proves the fix cannot degrade
// into the leak it replaces: if the box cannot hold the file, the session does
// not start with the credential on a command line instead.
func TestProvider_StartFailsClosedWhenStagingFails(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isStagingScript(argv) {
			return nil, 1, nil // e.g. read-only /tmp, or a short write
		}
		if isTmux("has-session")(argv) {
			return nil, 1, nil
		}
		return nil, 0, nil
	}}
	cfg := runtime.Config{Command: "agent", Env: map[string]string{"OPENAI_API_KEY": "sk-test-not-a-real-credential"}}
	err := providerWith(f).Start(context.Background(), "s", cfg)
	if err == nil {
		t.Fatal("Start must fail when the secret env cannot be staged")
	}
	if firstCall(f, isTmux("new-session")) != nil || firstCall(f, isTmux("start-server")) != nil {
		t.Error("no session may be launched once staging has failed")
	}
}

// TestProvider_RelaunchKeepsSecretEnvOutOfArgv covers the warm-box path: no -e
// is re-applied, but the setup prelude still has to export the credentials, and
// it must do it from the staged file rather than inline in `sh -c`.
func TestProvider_RelaunchKeepsSecretEnvOutOfArgv(t *testing.T) {
	const secret = "sk-test-not-a-real-credential"
	f := &fakeRunner{respond: answerStaging(nil)}
	cfg := runtime.Config{
		Command:      "agent --resumed",
		WorkDir:      "/w",
		Env:          map[string]string{"ANTHROPIC_AUTH_TOKEN": secret, "GC_RIG": "r"},
		SessionSetup: []string{"setup"},
	}
	if err := providerWith(f).Relaunch(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	for _, c := range f.calls {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Fatalf("secret value reached remote argv: %v", c)
			}
		}
	}
	setup := firstCall(f, func(argv []string) bool {
		return len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" && strings.HasSuffix(argv[2], "setup")
	})
	if setup == nil {
		t.Fatal("SessionSetup sh -c was not issued")
	}
	if !strings.Contains(setup[2], ". '"+stagedDir+"/env.sh'") {
		t.Errorf("prelude does not source the staged env file:\n%s", setup[2])
	}
	// Relaunch creates no session, so it stages no tmux command file.
	if strings.Contains(stdinFor(f, isStagingScript), "session.tmux") {
		t.Error("relaunch must not stage a tmux command file")
	}
	if firstCall(f, func(argv []string) bool {
		return slices.Equal(argv, []string{"rm", "-rf", stagedDir})
	}) == nil {
		t.Error("staged directory was not cleaned up")
	}
}

// TestProvider_RelaunchFailsClosedWhenStagingFails is Relaunch's half of the
// fail-closed contract.
func TestProvider_RelaunchFailsClosedWhenStagingFails(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isStagingScript(argv) {
			return nil, 1, nil
		}
		return nil, 0, nil
	}}
	cfg := runtime.Config{Command: "agent", Env: map[string]string{"OPENAI_API_KEY": "sk-test-not-a-real-credential"}}
	if err := providerWith(f).Relaunch(context.Background(), "s", cfg); err == nil {
		t.Fatal("Relaunch must fail when the secret env cannot be staged")
	}
	if firstCall(f, isTmux("respawn-pane")) != nil {
		t.Error("no agent may be relaunched once staging has failed")
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

func TestProvider_RelaunchRespawnsAgentInWarmSession(t *testing.T) {
	// Session exists (has-session → 0) and respawn-pane succeeds (default 0).
	f := &fakeRunner{respond: answerStaging(nil)}
	p := providerWith(f)
	cfg := runtime.Config{Command: "agent --resumed", WorkDir: "/w", Env: map[string]string{"A": "1"}}
	if err := p.Relaunch(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	got := firstCall(f, isTmux("respawn-pane"))
	// respawn-pane has no -e, so env is NOT re-applied (provision-half).
	want := []string{"tmux", "respawn-pane", "-k", "-t", "=s:@1.%1", "-c", "/w", "agent --resumed"}
	if !slices.Equal(got, want) {
		t.Errorf("respawn-pane argv =\n  %v\nwant\n  %v", got, want)
	}
	if firstCall(f, isTmux("new-session")) != nil {
		t.Error("Relaunch must reuse the warm session, not new-session")
	}
}

func TestProvider_RelaunchMissingSessionIsErrSessionNotFound(t *testing.T) {
	// has-session → 1 (no session): relaunch must error, not silently new-session.
	f := &fakeRunner{code: 1}
	p := providerWith(f)
	err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent"})
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Relaunch err = %v, want ErrSessionNotFound", err)
	}
	if firstCall(f, isTmux("respawn-pane")) != nil {
		t.Error("respawn-pane must not be issued when the session is absent")
	}
}

func TestProvider_RelaunchDeadAfterRespawnIsErrSessionDied(t *testing.T) {
	// Guard has-session → alive; after respawn the liveness recheck finds it dead.
	hasSessionCalls := 0
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("has-session")(argv) {
			hasSessionCalls++
			if hasSessionCalls == 1 {
				return nil, 0, nil // guard: warm session exists
			}
			return nil, 1, nil // liveness recheck: agent died immediately
		}
		return nil, 0, nil
	}}
	p := providerWith(f)
	cfg := runtime.Config{Command: "agent", ProcessNames: []string{"agent"}} // managed hints → liveness recheck
	err := p.Relaunch(context.Background(), "s", cfg)
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Relaunch err = %v, want ErrSessionDiedDuringStartup", err)
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

func TestProvider_StopReturnsTransportError(t *testing.T) {
	// A transport failure (ctx error or ssh exit 255) surfaces as err!=nil from
	// the runner. Stop must NOT swallow it: reporting success would let the seam
	// adapter drop tracking while the remote session keeps running untracked.
	want := errors.New("ssh box: connection failed (ssh exit 255)")
	f := &fakeRunner{code: -1, err: want}
	p := providerWith(f)
	err := p.Stop("s")
	if err == nil {
		t.Fatal("Stop must return the transport error, not swallow it")
	}
	if !errors.Is(err, want) {
		t.Fatalf("Stop err = %v, want wrapped %v", err, want)
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
	// The carrier targets the session's agent pane, resolved once for the
	// operation and fenced to the session (one host, many sessions).
	f := &fakeRunner{}
	p := providerWith(f)
	if err := p.Nudge("sess-7", runtime.TextContent("hi")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	want := [][]string{
		{"tmux", "list-panes", "-s", "-t", "=sess-7:", "-F", agentPaneRowFormat},
		{"tmux", "send-keys", "-t", "=sess-7:@1.%1", "-l", "hi"},
		{"tmux", "send-keys", "-t", "=sess-7:@1.%1", "Enter"},
	}
	if len(f.calls) != 3 {
		t.Fatalf("calls = %v, want 3", f.calls)
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
		Env:     map[string]string{"GC_ALIAS": "hello world", "GC_TEMPLATE": `a'b"c`},
	}
	if err := providerWith(f).Start(context.Background(), "sess-one", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := firstCall(f, isTmux("new-session"))
	want := []string{
		"tmux", "new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "sess-one",
		"-c", "/path with space",
		"-e", "GC_ALIAS=hello world",
		"-e", `GC_TEMPLATE=a'b"c`,
		`agent --flag "a b"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("new-session argv =\n  %#v\nwant\n  %#v", got, want)
	}
}

// TestProvider_StartQuotesStagedSecretEnv is the staged-file twin of
// TestProvider_StartQuotesNameWorkdirEnvCommand: the same awkward values must
// survive the trip through the tmux command file byte for byte.
func TestProvider_StartQuotesStagedSecretEnv(t *testing.T) {
	f := &fakeRunner{respond: answerStaging(func(argv []string) ([]byte, int, error) {
		if isTmux("has-session")(argv) {
			return nil, 1, nil
		}
		return nil, 0, nil
	})}
	cfg := runtime.Config{
		Command: `agent --flag "a b"`,
		WorkDir: "/path with space",
		Env:     map[string]string{"MSG": "hello world", "Q": "a'b\"c$d#e;f\\g\nh"},
	}
	if err := providerWith(f).Start(context.Background(), "sess-one", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	script := stdinFor(f, isStagingScript)
	wantLine := tmuxCommandLine([]string{
		"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "sess-one",
		"-c", "/path with space",
		"-e", "MSG=hello world",
		"-e", "Q=a'b\"c$d#e;f\\g\nh",
		`agent --flag "a b"`,
	})
	if !strings.Contains(script, wantLine) {
		t.Errorf("staged tmux command file missing the quoted new-session line:\n%s\nwant:\n%s", script, wantLine)
	}
	if !strings.Contains(script, "Q="+shellQuote([]string{"a'b\"c$d#e;f\\g\nh"})) {
		t.Errorf("staged sh env file missing the quoted value:\n%s", script)
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

// isProcessAliveScript matches ProcessAlive's one remote call.
func isProcessAliveScript(argv []string) bool {
	return len(argv) == 3 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], processSectionComm)
}

// processTable renders a processAliveScript answer: the session's panes
// (comma-separated "pid" for an unmarked session, or "<mark> pid"), then
// "pid ppid comm args..." rows split into the comm and args passes (comm is
// the third field, args the rest, or the comm when there are no more).
func processTable(panes string, rows ...string) string {
	var comm, args strings.Builder
	for _, row := range rows {
		f := strings.Fields(row)
		fmt.Fprintf(&comm, "%s %s %s\n", f[0], f[1], f[2])
		a := strings.Join(f[3:], " ")
		if a == "" {
			a = f[2]
		}
		fmt.Fprintf(&args, "%s %s %s\n", f[0], f[1], a)
	}
	var paneRows []string
	for _, pane := range strings.Split(panes, ",") {
		if pane != "" && !strings.Contains(pane, " ") {
			pane = "pane " + pane
		}
		paneRows = append(paneRows, pane)
	}
	return strings.Join(paneRows, "\n") + "\n" + processSectionComm + "\n" + comm.String() + processSectionArgs + "\n" + args.String()
}

// ProcessAlive is scoped to the session's pane process trees. A box-wide
// match read a dead session alive whenever another session on the box ran the
// same CLI (mc-pnu61). Matching follows the local provider: command, argv[0],
// or an interpreter's script, in a pane process or its descendants.
func TestProvider_ProcessAliveScopedToSessionPaneTree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		out   string
		code  int
		names []string
		want  bool
	}{
		{"agent under the pane shell", processTable("100", "100 1 zsh", "101 100 claude", "200 1 tmux"), 0, []string{"claude"}, true},
		{"agent deep in the tree", processTable("100", "100 1 zsh", "101 100 sh", "102 101 caffeinate", "103 102 claude"), 0, []string{"claude"}, true},
		{"another session's agent only", processTable("100", "100 1 zsh", "300 1 zsh", "301 300 claude"), 0, []string{"claude"}, false},
		{"node script by name", processTable("100", "100 1 zsh", "101 100 node node /usr/lib/gemini/bin/gemini --yolo"), 0, []string{"gemini"}, true},
		// Claude sets argv[0] to its version: only comm names it.
		{"comm only", processTable("100", "100 1 zsh", "101 100 claude 2.1.30 --resume"), 0, []string{"claude"}, true},
		{"any of several panes", processTable("100,400", "100 1 zsh", "400 1 zsh", "401 400 codex"), 0, []string{"codex"}, true},
		// A marked session is judged by its agent pane only: a human
		// running the CLI in a split does not keep a dead agent alive.
		{"marked: agent pane runs it", processTable("orphan 400,agent 100", "100 1 zsh", "101 100 codex", "400 1 zsh"), 0, []string{"codex"}, true},
		{"marked: only a human split runs it", processTable("agent 100,orphan 400", "100 1 zsh", "400 1 zsh", "401 400 codex"), 0, []string{"codex"}, false},
		{"marked: agent pane gone", processTable("orphan 400", "400 1 zsh", "401 400 codex"), 0, []string{"codex"}, false},
		{"marked: two agent panes, either runs it", processTable("agent 100,agent 400", "100 1 zsh", "400 1 zsh", "401 400 codex"), 0, []string{"codex"}, true},
		{"session missing", "", 3, []string{"claude"}, false},
		{"no panes listed", processTable("", "300 1 zsh", "301 300 claude"), 0, []string{"claude"}, false},
		{"process table unreadable", "pane 100\n" + processSectionComm + "\n" + processSectionArgs + "\n", 0, []string{"claude"}, true},
		// ps failed (exit 1, "unknown option"): the script's status is ps's,
		// but the session's panes exist, so it must not read dead (a dead
		// read makes every session on the box a zombie, killed every pass).
		{"ps fails", "pane 100\n" + processSectionComm + "\n" + processSectionArgs + "\n", 1, []string{"claude"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
				if isProcessAliveScript(argv) {
					return []byte(tc.out), tc.code, nil
				}
				return nil, 1, nil
			}}
			if got := providerWith(f).ProcessAlive("s", tc.names); got != tc.want {
				t.Errorf("ProcessAlive = %v, want %v", got, tc.want)
			}
			if len(f.calls) != 1 {
				t.Errorf("calls = %q, want one remote call", f.calls)
			}
		})
	}
}

// The remote call lists only this session's panes, exactly, and reads the
// process table with POSIX ps (any remote OS).
func TestProvider_ProcessAliveScriptTargetsSessionExactly(t *testing.T) {
	script := processAliveScript("worker-1")
	for _, want := range []string{
		"tmux list-panes -s -t '=worker-1:' -F '" + runtime.TmuxAgentPaneMarkerFormat + " #{pane_pid}') || exit 3",
		"ps -A -o pid= -o ppid= -o comm= | awk -v roots=",
		"ps -A -o pid= -o ppid= -o args= | awk -v roots=",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "pgrep") {
		t.Errorf("script still runs a box-wide pgrep:\n%s", script)
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

// TestProvider_PreStartFailureOmitsCredentials pins that a pre_start failure
// keeps credentials out of its message. Both halves it renders can carry one:
// the command may name a credential inline, and the prelude exported the
// session env to the box, so a `set -x` trace or a failing curl echoes it
// straight back. Unlike argv, this error is durable — logs, the event bus and
// bead notes — so a value that lands in it stays there.
//
// Only the session env is in scope. The command ran on the far box and
// inherited that box's environment, not this process's, which is why this path
// takes runtime.SecretEnvValues rather than the host-side SetupCommandSecrets.
func TestProvider_PreStartFailureOmitsCredentials(t *testing.T) {
	const sentinel = "sk-test-NOT-A-REAL-CREDENTIAL-8f3a21"
	f := &fakeRunner{respond: answerStaging(func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			return nil, 1, nil
		case len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], "curl"):
			// What a `set -x` trace looks like: the box echoing back the value
			// the prelude just exported to it.
			return []byte("+ curl -H 'Authorization: Bearer " + sentinel + "'\ncurl: (22) 401"), 3, nil
		}
		return nil, 0, nil
	})}
	err := providerWith(f).Start(context.Background(), "s", runtime.Config{
		Command: "agent",
		// Both shapes at once: a credential written into the command inline —
		// pre_start is user-authored config, so nothing stops one landing there
		// — and a reference to one the prelude exported.
		PreStart: []string{"curl -H \"Authorization: Bearer " + sentinel + "\" -u \"$ANTHROPIC_AUTH_TOKEN\" https://x"},
		Env:      map[string]string{"ANTHROPIC_AUTH_TOKEN": sentinel, "GC_RIG": "hauler"},
	})
	if err == nil {
		t.Fatal("Start must abort when a PreStart command fails")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("a credential reached the pre_start failure: %v", err)
	}
	// The controls. Without these the assertion above passes on an error that
	// rendered neither the command nor the output — including one where the
	// command never ran, and one over-redacted into uselessness.
	if !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("the command did not fail as expected: %v", err)
	}
	for _, want := range []string{"curl: (22) 401", "Authorization: Bearer " + runtime.RedactedValue} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure detail missing %q, so nothing here was scrubbed: %v", want, err)
		}
	}
	// An argv-safe value stays legible: hiding it costs diagnostics and buys
	// nothing, since it is already readable in /proc/<pid>/cmdline.
	if !strings.Contains(err.Error(), "$ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("the command's variable reference was scrubbed, which is the diagnostic: %v", err)
	}
}

// TestProvider_PreStartTransportFailureOmitsCredentials covers the other way a
// pre_start failure arrives, which is not the one it looks like.
//
// ssh reserves exit 255 for its own failures and cannot distinguish those from a
// remote command that genuinely exits 255, so shellRunner.run collapses both
// into a transport error and folds the box's stderr into its message. That
// stderr belongs to the pre_start command, written after the prelude exported
// the session env — so this branch is a credential channel that merely looks
// like a connectivity one, and it reaches the same durable places.
func TestProvider_PreStartTransportFailureOmitsCredentials(t *testing.T) {
	const sentinel = "sk-test-NOT-A-REAL-CREDENTIAL-8f3a21"
	f := &fakeRunner{respond: answerStaging(func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			return nil, 1, nil
		case len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], "curl"):
			// Shaped exactly as shellRunner.run's 255 collapse renders it: the
			// remote stderr verbatim, with no exit code to signal it was the
			// command's own result.
			return nil, -1, fmt.Errorf("ssh box: + curl -H 'Authorization: Bearer %s'\nKilled by signal 1", sentinel)
		}
		return nil, 0, nil
	})}
	err := providerWith(f).Start(context.Background(), "s", runtime.Config{
		Command:  "agent",
		PreStart: []string{"curl -u \"$ANTHROPIC_AUTH_TOKEN\" https://x"},
		Env:      map[string]string{"ANTHROPIC_AUTH_TOKEN": sentinel, "GC_RIG": "hauler"},
	})
	if err == nil {
		t.Fatal("Start must abort when a PreStart command fails at the transport")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("a credential reached the pre_start transport failure: %v", err)
	}
	// The controls: the failure must still say what broke and where, or this
	// passes on an error that carried no remote output to leak in the first
	// place — and on one over-redacted into uselessness.
	for _, want := range []string{"Killed by signal 1", "Authorization: Bearer " + runtime.RedactedValue, "$ANTHROPIC_AUTH_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the transport failure lost %q: %v", want, err)
		}
	}
}

// TestProvider_PreStartCancellationStaysMatchable pins the one exemption to the
// rule above. Redacting means rendering rather than wrapping, which costs the
// error chain — acceptable for a message nothing matches on, but cancellation
// is matched on, and shellRunner.run returns it before reading any stderr, so
// that branch has nothing to redact and keeps its %w.
func TestProvider_PreStartCancellationStaysMatchable(t *testing.T) {
	f := &fakeRunner{respond: answerStaging(func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			return nil, 1, nil
		case len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], "curl"):
			return nil, -1, fmt.Errorf("ssh box: %w", context.DeadlineExceeded)
		}
		return nil, 0, nil
	})}
	err := providerWith(f).Start(context.Background(), "s", runtime.Config{
		Command:  "agent",
		PreStart: []string{"curl https://x"},
		Env:      map[string]string{"GC_RIG": "hauler"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a canceled pre_start must stay matchable, got %v", err)
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
	// Inert env stays inline in the prelude; the staged-file path for secret
	// values is covered by TestProvider_StartKeepsSecretEnvOutOfArgv.
	cfg := runtime.Config{Command: "agent", WorkDir: "/w space", Env: map[string]string{"GC_CITY": "bar baz"}, PreStart: []string{"prep"}}
	if err := providerWith(f).Start(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if pre == nil {
		t.Fatal("PreStart sh -c was not issued")
	}
	for _, want := range []string{`cd '/w space' || exit 1`, `export GC_CITY='bar baz'`, `export GC_SESSION='s'`, "prep"} {
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
	if !slices.Equal(nudges[0], []string{"tmux", "send-keys", "-t", "=s:@1.%1", "-l", "hello"}) {
		t.Errorf("initial nudge type = %v", nudges[0])
	}
}

func TestProvider_AttachArgsQuotesRemoteCommand(t *testing.T) {
	// A session name with shell metacharacters must be confined to a single
	// shell-quoted remote-command argument — no remote command injection.
	args := attachArgs(Endpoint{User: "u", Host: "box"}, "x; rm -rf ~")
	// Attach rejects such a name (checkName); the quoting holds regardless.
	if last := args[len(args)-1]; last != `'tmux' 'attach' '-t' '=x; rm -rf ~'` {
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

// TestProvider_SetMetaKeepsSecretValueOutOfArgv: a secret-classified meta value
// is staged on the box over stdin and applied with source-file; it reaches no
// command line on either end.
func TestProvider_SetMetaKeepsSecretValueOutOfArgv(t *testing.T) {
	const secret = "instance-token-not-a-real-credential"
	f := &fakeRunner{respond: answerStaging(nil)}
	if err := providerWith(f).SetMeta("s", "GC_INSTANCE_TOKEN", secret); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	for _, c := range f.calls {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Fatalf("secret meta value reached remote argv: %v", c)
			}
		}
	}
	if firstCall(f, isTmux("set-environment")) != nil {
		t.Error("set-environment must not be issued as argv for a secret value")
	}
	got := firstCall(f, isTmux("source-file"))
	want := []string{"tmux", "source-file", stagedDir + "/session.tmux"}
	if !slices.Equal(got, want) {
		t.Errorf("apply argv =\n  %v\nwant\n  %v", got, want)
	}
	if !strings.Contains(stdinFor(f, isStagingScript), secret) {
		t.Error("secret value was not delivered on the staging script's stdin")
	}
	// The staged command names the session exactly, like the argv path.
	if staged := stdinFor(f, isStagingScript); !strings.Contains(staged, "'set-environment' '-t' '=s'") {
		t.Errorf("staged command does not target =s exactly: %q", staged)
	}
	if firstCall(f, func(argv []string) bool { return len(argv) == 3 && argv[0] == "rm" && argv[2] == stagedDir }) == nil {
		t.Error("staged directory was not removed after SetMeta")
	}
}

// TestProvider_SetMetaSkipsWhenStagingFails: best-effort must never mean
// falling back to argv.
func TestProvider_SetMetaSkipsWhenStagingFails(t *testing.T) {
	const secret = "instance-token-not-a-real-credential"
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isStagingScript(argv) {
			return nil, 1, nil
		}
		return nil, 0, nil
	}}
	_ = providerWith(f).SetMeta("s", "GC_INSTANCE_TOKEN", secret)
	for _, c := range f.calls {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Fatalf("secret meta value reached remote argv after staging failed: %v", c)
			}
		}
	}
}

// TestProvider_SetMetaInertValueStaysOnArgv keeps the common path file-free.
func TestProvider_SetMetaInertValueStaysOnArgv(t *testing.T) {
	f := &fakeRunner{}
	if err := providerWith(f).SetMeta("s", "GC_SESSION_ID", "gc-1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	want := []string{"tmux", "set-environment", "-t", "=s", "GC_SESSION_ID", "gc-1"}
	if got := firstCall(f, isTmux("set-environment")); !slices.Equal(got, want) {
		t.Errorf("set-environment argv = %v, want %v", got, want)
	}
	if firstCall(f, isStagingScript) != nil {
		t.Error("an inert meta value must not stage a file on the box")
	}
}

// tmux resolves a bare -t name by unique prefix: once "worker-1" has exited,
// "-t worker-1" names a live "worker-10" on the same box. Every session-keyed
// command must name its session exactly: "=name" for target-session commands,
// "=name:" for target-window and target-pane ones (tmux still prefix-matches
// "=name" there).
func TestProvider_TargetsSessionsExactly(t *testing.T) {
	const session, pane, agent = "=worker-1", "=worker-1:", "=worker-1:@1.%1"
	cases := []struct {
		name string
		run  func(*Provider)
		sub  string
		want string
	}{
		{"IsRunning", func(p *Provider) { _ = p.IsRunning("worker-1") }, "has-session", session},
		{"Stop", func(p *Provider) { _ = p.Stop("worker-1") }, "kill-session", session},
		{"IsAttached", func(p *Provider) { _ = p.IsAttached("worker-1") }, "display-message", pane},
		{"GetLastActivity", func(p *Provider) { _, _ = p.GetLastActivity("worker-1") }, "display-message", pane},
		{"SetMeta", func(p *Provider) { _ = p.SetMeta("worker-1", "GC_SESSION_ID", "gc-1") }, "set-environment", session},
		{"GetMeta", func(p *Provider) { _, _ = p.GetMeta("worker-1", "GC_K") }, "show-environment", session},
		{"RemoveMeta", func(p *Provider) { _ = p.RemoveMeta("worker-1", "GC_K") }, "set-environment", session},
		{"Relaunch", func(p *Provider) { _ = p.Relaunch(context.Background(), "worker-1", runtime.Config{Command: "agent"}) }, "respawn-pane", agent},
		{"Nudge", func(p *Provider) { _ = p.Nudge("worker-1", runtime.TextContent("hi")) }, "send-keys", agent},
		{"SendKeys", func(p *Provider) { _ = p.SendKeys("worker-1", "Enter") }, "send-keys", agent},
		{"Interrupt", func(p *Provider) { _ = p.Interrupt("worker-1") }, "send-keys", agent},
		{"Peek", func(p *Provider) { _, _ = p.Peek("worker-1", 5) }, "capture-pane", agent},
		{"ClearScrollback", func(p *Provider) { _ = p.ClearScrollback("worker-1") }, "clear-history", agent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{}
			tc.run(providerWith(f))
			var targets []string
			for _, c := range f.calls {
				if len(c) < 2 || c[0] != "tmux" {
					continue
				}
				if i := slices.Index(c, "-t"); i > 0 && i+1 < len(c) {
					targets = append(targets, c[1]+" "+c[i+1])
					if c[1] == tc.sub && c[i+1] != tc.want {
						t.Errorf("%s -t %q, want %q; argv=%q", c[1], c[i+1], tc.want, c)
					}
				}
			}
			if !slices.Contains(targets, tc.sub+" "+tc.want) {
				t.Errorf("no %s -t %s; targeted calls=%q", tc.sub, tc.want, targets)
			}
		})
	}
}

// Attach names the session exactly too, shell-quoted so a remote login shell
// (zsh expands a leading "=" as a command lookup) passes it to tmux intact.
func TestProvider_AttachArgsTargetSessionExactly(t *testing.T) {
	args := attachArgs(Endpoint{User: "u", Host: "box"}, "worker-1")
	if last := args[len(args)-1]; last != `'tmux' 'attach' '-t' '=worker-1'` {
		t.Errorf("remote command arg = %q, want an exact, quoted target", last)
	}
}

// A name that is not a safe tmux target never reaches tmux. A bare target is a
// prefix match and a glob ("worker-1*" names worker-10), and "." and ":" split
// it into window and pane parts (mc-xm7mg). Verbs that return errors report
// ErrInvalidSessionName; best-effort verbs do nothing.
func TestProvider_InvalidNamesNeverReachTmux(t *testing.T) {
	for _, name := range []string{"worker-1*", "worker-?", "worker-1:0", "worker.1", "", "x; rm -rf ~"} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRunner{}
			p := providerWith(f)
			ctx := context.Background()
			for verb, err := range map[string]error{
				"Start":    p.Start(ctx, name, runtime.Config{Command: "agent"}),
				"Relaunch": p.Relaunch(ctx, name, runtime.Config{Command: "agent"}),
				"Stop":     p.Stop(name),
				"Attach":   p.Attach(name),
			} {
				if !errors.Is(err, ErrInvalidSessionName) {
					t.Errorf("%s(%q) = %v, want ErrInvalidSessionName", verb, name, err)
				}
			}
			if p.IsRunning(name) || p.IsAttached(name) || p.ProcessAlive(name, []string{"claude"}) {
				t.Errorf("IsRunning/IsAttached/ProcessAlive(%q) reported true", name)
			}
			_ = p.Nudge(name, runtime.TextContent("hi"))
			_ = p.SendKeys(name, "Enter")
			_ = p.Interrupt(name)
			_ = p.ClearScrollback(name)
			_, _ = p.Peek(name, 5)
			_ = p.SetMeta(name, "GC_SESSION_ID", "gc-1")
			_, _ = p.GetMeta(name, "GC_SESSION_ID")
			_ = p.RemoveMeta(name, "GC_SESSION_ID")
			_, _ = p.GetLastActivity(name)
			if len(f.calls) != 0 {
				t.Errorf("remote calls for %q = %q, want none", name, f.calls)
			}
		})
	}
}

// A degraded process read (ps failing on the box) is logged once per episode,
// not on every tick, and a good read ends the episode.
func TestProvider_ProcessAliveLogsDegradedOncePerEpisode(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	degraded := "pane 100\n" + processSectionComm + "\n" + processSectionArgs + "\n"
	healthy := processTable("100", "100 1 zsh", "101 100 claude")
	out := degraded
	f := &fakeRunner{respond: func([]string) ([]byte, int, error) { return []byte(out), 1, nil }}
	p := providerWith(f)
	for i := 0; i < 3; i++ {
		if !p.ProcessAlive("s", []string{"claude"}) {
			t.Fatal("degraded ProcessAlive = false, want true")
		}
	}
	if n := strings.Count(logged.String(), "process snapshot degraded"); n != 1 {
		t.Fatalf("degraded logs = %d, want 1:\n%s", n, logged.String())
	}
	out = healthy
	_ = p.ProcessAlive("s", []string{"claude"})
	out = degraded
	_ = p.ProcessAlive("s", []string{"claude"})
	if n := strings.Count(logged.String(), "process snapshot degraded"); n != 2 {
		t.Fatalf("degraded logs after a new episode = %d, want 2", n)
	}
}

// Start marks the pane new-session printed as the session's agent pane,
// valued with the session's id, and only then flags the session as marked.
func TestProvider_StartMarksAgentPane(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			return nil, 1, nil
		case isTmux("new-session")(argv):
			return []byte("%7\n"), 0, nil
		}
		return nil, 0, nil
	}}
	if err := providerWith(f).Start(context.Background(), "s", runtime.Config{Command: "agent"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mark := []string{"tmux", "set-option", "-p", "-F", "-t", "%7", runtime.TmuxAgentPaneOption, "#{session_id}"}
	flag := []string{"tmux", "set-option", "-t", "=s:", runtime.TmuxAgentSessionOption, "1"}
	created := slices.IndexFunc(f.calls, isTmux("new-session"))
	marked := slices.IndexFunc(f.calls, func(c []string) bool { return slices.Equal(c, mark) })
	flagged := slices.IndexFunc(f.calls, func(c []string) bool { return slices.Equal(c, flag) })
	if created < 0 || marked <= created || flagged <= marked {
		t.Fatalf("calls = %q, want new-session, then %q, then %q", f.calls, mark, flag)
	}
}

// Relaunch and the driving verbs reach the session's agent pane, never its
// active pane, and fail closed when the agent pane cannot be named: a human
// killed it (the session lives on in their panes), or two panes claim it.
func TestProvider_AgentPaneResolution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		panes   string
		want    string // respawn-pane / send-keys target; "" when nothing may be sent
		wantErr error
	}{
		{"marked pane behind a human split", "orphan\t@1.%3\nagent\t@2.%5\norphan\t@2.%6", "=s:@2.%5", nil},
		{"unmarked session takes the first pane", "pane\t@1.%3\npane\t@1.%4", "=s:@1.%3", nil},
		{"agent pane killed", "orphan\t@1.%3\norphan\t@1.%4", "", runtime.ErrTmuxAgentPaneGone},
		{"two agent panes", "agent\t@1.%3\nagent\t@2.%4", "", runtime.ErrTmuxAgentPaneAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{panes: tc.panes}
			p := providerWith(f)
			err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent"})
			_ = p.Nudge("s", runtime.TextContent("hi"))
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Relaunch = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && err != nil {
				t.Errorf("Relaunch = %v, want nil", err)
			}
			for _, c := range f.calls {
				if len(c) < 2 || c[0] != "tmux" || (c[1] != "respawn-pane" && c[1] != "send-keys") {
					continue
				}
				if i := slices.Index(c, "-t"); tc.want == "" || c[i+1] != tc.want {
					t.Errorf("%s -t %q, want %q; argv=%q", c[1], c[i+1], tc.want, c)
				}
			}
		})
	}
}

// Relaunch marks a warm box from before the marker while it has only the
// agent's pane, and leaves any other session alone.
func TestProvider_RelaunchAdoptsLoneUnmarkedPane(t *testing.T) {
	for _, tc := range []struct {
		name     string
		panes    string
		wantMark bool
	}{
		{"lone unmarked pane", "pane\t@1.%3", true},
		{"two unmarked panes", "pane\t@1.%3\npane\t@1.%4", false},
		{"already marked", "agent\t@1.%3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{panes: tc.panes}
			if err := providerWith(f).Relaunch(context.Background(), "s", runtime.Config{Command: "agent"}); err != nil {
				t.Fatalf("Relaunch: %v", err)
			}
			marked := slices.ContainsFunc(f.calls, func(c []string) bool {
				return slices.Equal(c, []string{"tmux", "set-option", "-p", "-F", "-t", "%3", runtime.TmuxAgentPaneOption, "#{session_id}"})
			})
			if marked != tc.wantMark {
				t.Errorf("marked = %v, want %v; calls=%q", marked, tc.wantMark, f.calls)
			}
		})
	}
}

// An agent-pane lookup that names no pane fails closed: a missing session is
// ErrSessionNotFound, and output with no pane row never falls back to the
// session's active pane ("=s:").
func TestProvider_AgentPaneLookupFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		code    int
		wantErr error
	}{
		{"session missing", "", 1, runtime.ErrSessionNotFound},
		{"no pane rows", "", 0, nil},
		{"malformed row", "agent\tgarbage", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{rawPanes: true, respond: func(argv []string) ([]byte, int, error) {
				if isAgentPaneLookup(argv) {
					return []byte(tc.out), tc.code, nil
				}
				return nil, 0, nil
			}}
			p := providerWith(f)
			_, err := p.agentPane(context.Background(), "s", false)
			if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Errorf("agentPane = %v, want an error (%v)", err, tc.wantErr)
			}
			_ = p.Nudge("s", runtime.TextContent("hi"))
			_ = p.SendKeys("s", "Enter")
			_ = p.Interrupt("s")
			_ = p.ClearScrollback("s")
			if out, err := p.Peek("s", 5); out != "" || err != nil {
				t.Errorf("Peek = (%q, %v), want (\"\", nil)", out, err)
			}
			for _, sub := range []string{"send-keys", "capture-pane", "clear-history"} {
				if c := firstCall(f, isTmux(sub)); c != nil {
					t.Errorf("%s ran after a failed lookup: %q", sub, c)
				}
			}
		})
	}
}

// A flagged (marked) session whose agent pane a human killed, leaving one
// human pane: Relaunch must not adopt that pane as the agent's and respawn it
// (killing the human's shell). It reports the agent pane gone and changes
// nothing.
func TestProvider_RelaunchRefusesLoneOrphanPane(t *testing.T) {
	f := &fakeRunner{panes: "orphan\t@1.%3"}
	err := providerWith(f).Relaunch(context.Background(), "s", runtime.Config{Command: "agent"})
	if !errors.Is(err, runtime.ErrTmuxAgentPaneGone) {
		t.Errorf("Relaunch = %v, want ErrTmuxAgentPaneGone", err)
	}
	for _, sub := range []string{"set-option", "respawn-pane"} {
		if c := firstCall(f, isTmux(sub)); c != nil {
			t.Errorf("%s ran on a lone orphan pane: %q", sub, c)
		}
	}
}

// A failed mark is logged and leaves the session unflagged: a flagged session
// without a marked pane would read every pane "orphan" and refuse all input
// until it is recreated.
func TestProvider_FailedMarkIsLoggedAndUnflagged(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			return nil, 1, nil
		case isTmux("new-session")(argv):
			return []byte("%7\n"), 0, nil
		case isTmux("set-option")(argv) && slices.Contains(argv, "-p"):
			return nil, 1, nil // tmux too old, or refused
		}
		return nil, 0, nil
	}}
	if err := providerWith(f).Start(context.Background(), "s", runtime.Config{Command: "agent"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.Contains(logged.String(), "marking agent pane") {
		t.Errorf("log = %q, want the failed mark logged", logged.String())
	}
	for _, c := range f.calls {
		if slices.Contains(c, runtime.TmuxAgentSessionOption) {
			t.Errorf("session flagged after a failed mark: %q", c)
		}
	}
}

// Only Relaunch adopts a lone unmarked pane; the driving verbs read and never
// write the marker.
func TestProvider_DrivingVerbsDoNotMark(t *testing.T) {
	f := &fakeRunner{panes: "pane\t@1.%3"}
	p := providerWith(f)
	_ = p.Nudge("s", runtime.TextContent("hi"))
	_, _ = p.Peek("s", 5)
	if c := firstCall(f, isTmux("set-option")); c != nil {
		t.Errorf("a driving verb marked the pane: %q", c)
	}
}
