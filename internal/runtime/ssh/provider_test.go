package ssh

import (
	"context"
	"errors"
	"fmt"
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
	want := []string{"tmux", "new-session", "-d", "-s", "s", "-c", "/w", "-e", "GC_AGENT=1", "-e", "GC_RIG=2", "agent --serve"}
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
func TestProvider_RelaunchRejectsMalformedCopyBeforeRemoteCalls(t *testing.T) {
	f := &fakeRunner{}
	p := providerWith(f)

	err := p.Relaunch(context.Background(), "s", runtime.Config{
		Command:   "agent --resumed",
		CopyFiles: []runtime.CopyEntry{{Src: "/seed", RelDst: "../outside"}},
	})
	if err == nil {
		t.Fatal("Relaunch() succeeded, want malformed copy destination error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("remote calls before staging preflight = %v, want none", f.calls)
	}
}

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
	want := []string{"tmux", "respawn-pane", "-k", "-t", "s", "-c", "/w", "agent --resumed"}
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
		Env:     map[string]string{"GC_ALIAS": "hello world", "GC_TEMPLATE": `a'b"c`},
	}
	if err := providerWith(f).Start(context.Background(), "sess-one", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := firstCall(f, isTmux("new-session"))
	want := []string{
		"tmux", "new-session", "-d", "-s", "sess-one",
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
		"new-session", "-d", "-s", "sess-one",
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
