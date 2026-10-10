package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBuildLaunchCommandUnsetsColorKillersForInteractiveExecutables(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		command  string
		want     string
	}{
		{name: "claude", provider: "claude", command: "claude", want: "env -u CI -u NO_COLOR claude"},
		{name: "claude alias", provider: "qlandia/claude", command: "claude", want: "env -u CI -u NO_COLOR claude"},
		{name: "claude without provider", command: "claude", want: "env -u CI -u NO_COLOR claude"},
		{name: "codex", provider: "codex", command: "codex", want: "env -u CI -u NO_COLOR codex"},
		{name: "kiro command", provider: "claude", command: "kiro-cli", want: "kiro-cli"},
		{name: "omp", provider: "omp", command: "omp", want: "omp"},
		{name: "custom", provider: "custom", command: "custom-agent", want: "custom-agent"},
		{name: "custom codex", provider: "custom-codex", command: "custom-codex", want: "custom-codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := buildLaunchCommand("worker", runtime.Config{Command: tc.command, ProviderName: tc.provider})
			if err != nil {
				t.Fatalf("buildLaunchCommand: %v", err)
			}
			if got != tc.want {
				t.Fatalf("command = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildLaunchCommandColorWrapsLongPromptCommand(t *testing.T) {
	got, promptFile, err := buildLaunchCommand("worker", runtime.Config{
		Command:      "/opt/bin/claude",
		ProviderName: "kiro",
		WorkDir:      t.TempDir(),
		PromptSuffix: strings.Repeat("prompt ", maxInlinePromptLen),
	})
	if err != nil {
		t.Fatalf("buildLaunchCommand: %v", err)
	}
	if promptFile == "" {
		t.Fatal("long prompt did not create a prompt file")
	}
	if !strings.HasPrefix(got, "env -u CI -u NO_COLOR sh -c ") {
		t.Fatalf("command = %q, want env wrapper around final sh -c command", got)
	}
}

func TestProviderAttachRefusesDeadPane(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"41", "", "1"},
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe
	p.tm.namedSocketLstat = stableNamedSocketLstat(t)

	err := p.Attach("runner")
	if err == nil {
		t.Fatal("Attach = nil, want dead pane error")
	}
	if !strings.Contains(err.Error(), "dead pane") {
		t.Fatalf("Attach error = %v, want dead pane context", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session for dead pane: %v", fe.calls)
		}
	}
}

func TestProviderAttachMissingSessionWrapsRuntimeSentinel(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"41", "", "41"},
		errs: []error{nil, ErrSessionNotFound, nil},
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe
	p.tm.namedSocketLstat = stableNamedSocketLstat(t)

	err := p.Attach("runner")
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Attach error = %v, want runtime.ErrSessionNotFound", err)
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Attach error = %v, want tmux ErrSessionNotFound", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session for missing session: %v", fe.calls)
		}
	}
}

func TestProviderListRunningReportsPartialOnNoServer(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe
	p.tm.serverSocketObserver = func(context.Context, string) error {
		// nil is observeNamedSocketWith's confirmed-absent contract (its
		// os.ErrNotExist and stale/absent-after-ECONNREFUSED branches both
		// return nil) — see server_socket_probe.go.
		return nil
	}

	names, err := p.ListRunning("")
	if names != nil {
		t.Fatalf("ListRunning names = %v, want nil on unreachable server", names)
	}
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning err = %v, want runtime.PartialListError so reconciler guards defer", err)
	}
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("ListRunning err = %v, want wrapped ErrNoServer cause", err)
	}
	if !runtime.IsRuntimeServerAbsent(err) {
		t.Fatalf("ListRunning err = %v, want ServerAbsent set when the socket observation corroborates absence", err)
	}
}

// TestProviderListRunningWithheldServerAbsentOnWedgedLiveServer is the #6176
// regression: a tmux server that answers ErrNoServer over the control
// protocol while its socket is still observable is wedged, not absent.
// ListRunning must not set ServerAbsent on protocol failure alone — that
// field gates the destructive reapPreBootSessionBeads sweep, and a
// misclassified absence on a long-uptime host can reap live sessions.
func TestProviderListRunningWithheldServerAbsentOnWedgedLiveServer(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe
	p.tm.serverSocketObserver = func(context.Context, string) error {
		// A non-nil error is observeNamedSocketWith's live/inconclusive
		// contract (its "reason=live-unix-socket" dial-succeeded branch, and
		// every fail-closed branch, return non-nil) — see
		// server_socket_probe.go and TestNewSessionErrNoServerRefusesObservedLiveNamedSocket,
		// which pins the identical shape for probeServerAlive's own use of
		// this observer.
		return errors.New("live socket path=x inode=97 peer_pid=4242")
	}

	names, err := p.ListRunning("")
	if names != nil {
		t.Fatalf("ListRunning names = %v, want nil on unreachable server", names)
	}
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning err = %v, want runtime.PartialListError so reconciler guards defer", err)
	}
	if runtime.IsRuntimeServerAbsent(err) {
		t.Fatalf("ListRunning err = %v, want ServerAbsent withheld: the socket is still observable, so this is a wedged server, not a dead one", err)
	}
}

func TestProviderListRunningPropagatesNonServerError(t *testing.T) {
	sentinel := errors.New("tmux exploded")
	fe := &fakeExecutor{err: sentinel}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	names, err := p.ListRunning("")
	if names != nil {
		t.Fatalf("ListRunning names = %v, want nil on error", names)
	}
	if runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning err = %v, want a plain error (not partial) for a real tmux failure", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("ListRunning err = %v, want the underlying tmux error", err)
	}
}

func TestProviderStopUnattendedSession(t *testing.T) {
	const (
		onePane = "$1\tworker\t@1\t%1\t0\t0\t0"
		twoPane = "$1\tworker\t@1\t%1\t0\t0\t0\n$1\tworker\t@2\t%2\t0\t0\t0"
	)
	sentinel := errors.New("tmux census failed")
	for _, test := range []struct {
		name               string
		before             string
		token              string
		after              string
		errs               []error
		wantOK             bool
		wantPaneIDs        []string
		emptyExpectedToken bool
		wantErrContains    string
		wantErrIs          error
		mustNotCallContain []string
	}{
		{name: "stable multiple windows and panes", before: twoPane, token: "GC_INSTANCE_TOKEN=token", after: twoPane, wantOK: true, wantPaneIDs: []string{"%1", "%2"}},
		{name: "attached client", before: "$1\tworker\t@1\t%1\t1\t0\t0", token: "GC_INSTANCE_TOKEN=token", after: onePane},
		{name: "multiple clients", before: "$1\tworker\t@1\t%1\t2\t0\t0", token: "GC_INSTANCE_TOKEN=token", after: onePane},
		{name: "linked window before token read", before: "$1\tworker\t@1\t%1\t0\t0\t1", token: "GC_INSTANCE_TOKEN=token", after: onePane, wantErrContains: "linked windows"},
		{name: "linked window after token read", before: onePane, token: "GC_INSTANCE_TOKEN=token", after: "$1\tworker\t@1\t%1\t0\t0\t1", wantErrContains: "linked windows"},
		{name: "copy mode on non-active pane", before: "$1\tworker\t@1\t%1\t0\t0\t0\n$1\tworker\t@2\t%2\t0\t1\t0", token: "GC_INSTANCE_TOKEN=token", after: twoPane},
		{name: "partial row", before: "$1\tworker\t@1\t%1\t0", token: "GC_INSTANCE_TOKEN=token", after: onePane},
		{name: "malformed count", before: "$1\tworker\t@1\t%1\tmany\t0\t0", token: "GC_INSTANCE_TOKEN=token", after: onePane},
		{name: "signed count", before: "$1\tworker\t@1\t%1\t+1\t0\t0", token: "GC_INSTANCE_TOKEN=token", after: onePane},
		{name: "duplicate pane", before: "$1\tworker\t@1\t%1\t0\t0\t0\n$1\tworker\t@1\t%1\t0\t0\t0", token: "GC_INSTANCE_TOKEN=token", after: onePane},
		{name: "mixed session IDs", before: "$1\tworker\t@1\t%1\t0\t0\t0\n$2\tworker\t@2\t%2\t0\t0\t0", token: "GC_INSTANCE_TOKEN=token", after: twoPane},
		{name: "wrong session name", before: "$1\treplacement\t@1\t%1\t0\t0\t0", token: "GC_INSTANCE_TOKEN=token", after: onePane},
		{name: "replacement between censuses", before: onePane, token: "GC_INSTANCE_TOKEN=token", after: "$2\tworker\t@1\t%1\t0\t0\t0"},
		{name: "pane topology replacement between censuses", before: onePane, token: "GC_INSTANCE_TOKEN=token", after: "$1\tworker\t@2\t%2\t0\t0\t0"},
		{name: "attachment after token read", before: onePane, token: "GC_INSTANCE_TOKEN=token", after: "$1\tworker\t@1\t%1\t1\t0\t0"},
		{name: "expected token missing", before: onePane, token: "GC_INSTANCE_TOKEN=token", after: onePane, emptyExpectedToken: true},
		{name: "token missing", before: onePane, token: "GC_INSTANCE_TOKEN=", after: onePane},
		{name: "token mismatch", before: onePane, token: "GC_INSTANCE_TOKEN=replacement", after: onePane},
		{name: "token read error", before: onePane, after: onePane, errs: []error{nil, sentinel}},
		{name: "first census error", errs: []error{sentinel}},
		{name: "second census error", before: onePane, token: "GC_INSTANCE_TOKEN=token", errs: []error{nil, nil, sentinel}},
		{name: "certified pane disappears", before: twoPane, token: "GC_INSTANCE_TOKEN=token", after: twoPane, errs: []error{nil, nil, nil, ErrSessionNotFound}, wantErrIs: ErrSessionNotFound, mustNotCallContain: []string{"%2", "kill-session"}},
		{name: "final exact session disappears", before: onePane, token: "GC_INSTANCE_TOKEN=token", after: onePane, errs: []error{nil, nil, nil, nil, ErrSessionNotFound}, wantOK: true, wantPaneIDs: []string{"%1"}},
		{name: "final kill finds no server", before: onePane, token: "GC_INSTANCE_TOKEN=token", after: onePane, errs: []error{nil, nil, nil, nil, ErrNoServer}, wantPaneIDs: []string{"%1"}, wantErrIs: ErrNoServer},
	} {
		t.Run(test.name, func(t *testing.T) {
			outputs := []string{test.before, test.token, test.after}
			if len(test.wantPaneIDs) > 0 {
				for range test.wantPaneIDs {
					outputs = append(outputs, "999999999\t0")
				}
				outputs = append(outputs, "")
			}
			fe := &fakeExecutor{outs: outputs, errs: test.errs}
			p := NewProviderWithConfig(Config{SocketName: "cert-socket"})
			p.tm.exec = fe

			expectedToken := "token"
			if test.emptyExpectedToken {
				expectedToken = ""
			}
			err := p.StopUnattendedSession("worker", expectedToken)
			if test.wantOK && err != nil {
				t.Fatalf("StopUnattendedSession: %v", err)
			}
			if !test.wantOK && err == nil {
				t.Fatal("StopUnattendedSession = nil, want fail-closed error")
			}
			if test.wantErrContains != "" && !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("StopUnattendedSession error = %v, want %q", err, test.wantErrContains)
			}
			if test.wantErrIs != nil && !errors.Is(err, test.wantErrIs) {
				t.Fatalf("StopUnattendedSession error = %v, want wrapped %v", err, test.wantErrIs)
			}
			for _, call := range fe.calls {
				joined := strings.Join(call, " ")
				for _, forbidden := range test.mustNotCallContain {
					if strings.Contains(joined, forbidden) {
						t.Fatalf("StopUnattendedSession continued after certified-pane loss: %v", call)
					}
				}
				if strings.Contains(joined, "detach-client") {
					t.Fatalf("certification detached an untracked client: %v", call)
				}
				if strings.Contains(joined, "list-panes") {
					want := []string{
						"-u", "-L", "cert-socket", "list-panes", "-s", "-t", "=worker", "-F",
						"#{session_id}\t#{session_name}\t#{window_id}\t#{pane_id}\t#{session_attached}\t#{pane_in_mode}\t#{window_linked}",
					}
					if !reflect.DeepEqual(call, want) {
						t.Fatalf("census argv = %#v, want %#v", call, want)
					}
				}
			}
			if len(test.wantPaneIDs) > 0 {
				wantTail := make([][]string, 0, len(test.wantPaneIDs)+1)
				for _, paneID := range test.wantPaneIDs {
					wantTail = append(wantTail, []string{"-u", "-L", "cert-socket", "display-message", "-t", paneID, "-p", "#{pane_pid}\t#{pane_dead}"})
				}
				wantTail = append(wantTail, []string{"-u", "-L", "cert-socket", "kill-session", "-t", "$1"})
				gotTail := fe.calls[len(fe.calls)-len(wantTail):]
				if !reflect.DeepEqual(gotTail, wantTail) {
					t.Fatalf("bound stop tail argv = %#v, want %#v", gotTail, wantTail)
				}
			}
		})
	}
}

func TestProviderStopUnattendedSessionLaterCertifiedPaneLossDoesNotTerminateEarlierPane(t *testing.T) {
	const twoPane = "$1\tworker\t@1\t%1\t0\t0\t0\n$1\tworker\t@2\t%2\t0\t0\t0"
	fe := &fakeExecutor{
		outs: []string{
			twoPane,
			"GC_INSTANCE_TOKEN=token",
			twoPane,
			"42424242\t0",
			"",
		},
		errs: []error{nil, nil, nil, nil, ErrSessionNotFound},
	}
	p := NewProviderWithConfig(Config{SocketName: "cert-socket"})
	p.tm.exec = fe
	var terminated []processKillPlan
	p.tm.certifiedPaneTerminator = func(plan processKillPlan) error {
		terminated = append(terminated, plan)
		return nil
	}

	err := p.StopUnattendedSession("worker", "token")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("StopUnattendedSession error = %v, want wrapped ErrSessionNotFound", err)
	}
	if len(terminated) != 0 {
		t.Fatalf("first certified pane was terminated before later lookup failed: %+v", terminated)
	}
	for _, call := range fe.calls {
		if slices.Contains(call, "kill-session") {
			t.Fatalf("session kill followed failed certified-pane lookup: %v", call)
		}
	}
}

type certificationWriteCloser struct {
	closed bool
}

func (*certificationWriteCloser) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w *certificationWriteCloser) Close() error {
	w.closed = true
	return nil
}

func TestProviderStopUnattendedSessionClosesOnlyTrackedHiddenClient(t *testing.T) {
	fe := &fakeExecutor{outs: []string{
		"$1\tworker\t@1\t%1\t0\t0\t0",
		"GC_INSTANCE_TOKEN=token",
		"$1\tworker\t@1\t%1\t0\t0\t0",
		"999999999\t0",
		"",
	}}
	p := NewProviderWithConfig(Config{SocketName: "cert-socket"})
	p.tm.exec = fe
	targetDone := make(chan error)
	close(targetDone)
	otherDone := make(chan error)
	close(otherDone)
	targetWriter := &certificationWriteCloser{}
	otherWriter := &certificationWriteCloser{}
	p.tm.hiddenAttachClients = map[string]*hiddenAttachClient{
		"worker": {cancel: func() {}, done: targetDone, stdin: targetWriter},
		"other":  {cancel: func() {}, done: otherDone, stdin: otherWriter},
	}

	if err := p.StopUnattendedSession("worker", "token"); err != nil {
		t.Fatalf("StopUnattendedSession: %v", err)
	}
	if !targetWriter.closed {
		t.Fatal("tracked hidden client was not closed before bound stop")
	}
	if otherWriter.closed || p.tm.hiddenAttachClient("other") == nil {
		t.Fatal("bound stop disturbed another tracked client")
	}
	if p.tm.hiddenAttachClient("worker") != nil {
		t.Fatal("closed hidden client remained tracked")
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "detach-client") {
			t.Fatalf("bound stop used detach-client: %v", call)
		}
	}
	p.tm.CloseHiddenAttachClient("other")
}

// TestListSessionsAbsorbsNoServer pins the tmux-internal contract that the
// change deliberately preserves: ListSessions still reports an unreachable
// server as an empty result so FindSessionByWorkDir and CleanupOrphanedSessions
// keep treating "server down" as "no sessions". Only Provider.ListRunning
// surfaces the outage as a PartialListError.
func TestListSessionsAbsorbsNoServer(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	tm := NewTmux()
	tm.exec = fe

	names, err := tm.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions err = %v, want nil (no server absorbed)", err)
	}
	if names != nil {
		t.Fatalf("ListSessions names = %v, want nil", names)
	}
}

func TestProviderAttachReportsHasSessionError(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"41"},
		errs: []error{nil, errors.New("tmux unavailable")},
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe
	p.tm.namedSocketLstat = stableNamedSocketLstat(t)

	err := p.Attach("runner")
	if err == nil {
		t.Fatal("Attach = nil, want has-session error")
	}
	if !strings.Contains(err.Error(), "checking tmux session before attach") {
		t.Fatalf("Attach error = %v, want checking context", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session after has-session error: %v", fe.calls)
		}
	}
}

func TestProviderAttachNamedSocketNoServerPreflightRefusesBeforeLaunchingTmux(t *testing.T) {
	for _, tc := range []struct {
		name        string
		observation namedSocketObservation
		lstatErr    error
		wantErr     error
		notErr      error
		want        string
	}{
		// A missing socket is the socket policy's clean "no server can be
		// listening" observation, so a stopped city reads as an absent session;
		// only an ambiguous observation reports a degraded server.
		{name: "missing", lstatErr: os.ErrNotExist, wantErr: runtime.ErrSessionNotFound, notErr: ErrServerDegraded, want: "reason=socket-missing"},
		{name: "non-socket", wantErr: ErrServerDegraded, notErr: runtime.ErrSessionNotFound, want: "reason=not-unix-socket"},
		{name: "stat-error", lstatErr: os.ErrPermission, wantErr: ErrServerDegraded, notErr: runtime.ErrSessionNotFound, want: "reason=socket-lstat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			invocations := filepath.Join(binDir, "tmux-invocations")
			fakeTmux := filepath.Join(binDir, "tmux")
			if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+invocations+"\n"), 0o755); err != nil {
				t.Fatalf("write fake tmux: %v", err)
			}
			t.Setenv("PATH", binDir)
			node := stableNamedSocketNode(t)
			if tc.name == "non-socket" {
				tc.observation = namedSocketObservation{node: node}
			}

			p := NewProviderWithConfig(Config{SocketName: "city-socket"})
			p.tm.exec = &fakeExecutor{
				outs: []string{"", "", ""},
				errs: []error{nil, nil, ErrNoServer},
			}
			p.tm.namedSocketLstat = func(context.Context, string) (namedSocketObservation, error) {
				return tc.observation, tc.lstatErr
			}

			err := p.Attach("runner")
			if !errors.Is(err, tc.wantErr) || errors.Is(err, tc.notErr) {
				t.Fatalf("Attach error = %v, want %v and not %v", err, tc.wantErr, tc.notErr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Attach error = %q, want %q", err, tc.want)
			}
			if output, readErr := os.ReadFile(invocations); readErr == nil && len(output) != 0 {
				t.Fatalf("Attach launched tmux after named-socket preflight failed: %s", output)
			} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("read fake tmux invocations: %v", readErr)
			}

			if got := len(p.tm.exec.(*fakeExecutor).calls); got != 0 {
				t.Fatalf("tmux calls = %d, want none after initial injected witness refusal", got)
			}
		})
	}
}

func TestNamedSocketAttachPreflightRefusesUnavailableServer(t *testing.T) {
	newTmux := func(errs []error) *Tmux {
		tm := &Tmux{
			cfg:  Config{SocketName: "gc-test"},
			exec: &fakeExecutor{errs: errs},
		}
		tm.namedSocketLstat = func(context.Context, string) (namedSocketObservation, error) {
			return namedSocketObservation{}, os.ErrNotExist
		}
		return tm
	}

	t.Run("direct", func(t *testing.T) {
		tm := newTmux(nil)
		err := tm.AttachSession("runner")
		if !errors.Is(err, ErrServerDegraded) {
			t.Fatalf("AttachSession error = %v, want ErrServerDegraded", err)
		}
		if got := len(tm.exec.(*fakeExecutor).calls); got != 0 {
			t.Fatalf("tmux calls = %d, want none after injected witness refusal", got)
		}
	})

	t.Run("hidden", func(t *testing.T) {
		tm := newTmux([]error{nil})
		err := tm.ensureHiddenAttachedClient("runner")
		if !errors.Is(err, ErrServerDegraded) {
			t.Fatalf("ensureHiddenAttachedClient error = %v, want ErrServerDegraded", err)
		}
		if got := len(tm.exec.(*fakeExecutor).calls); got != 0 {
			t.Fatalf("tmux calls = %d, want none after initial injected witness refusal", got)
		}
	})
}

func TestDefaultSocketAttachBypassesWitness(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{
		exec: fe,
		namedSocketLstat: func(context.Context, string) (namedSocketObservation, error) {
			t.Fatal("default socket must not lstat an attach witness")
			return namedSocketObservation{}, nil
		},
	}
	if err := tm.AttachSession("runner"); err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	if got, want := fe.calls, [][]string{{"-u", "attach-session", "-t", "runner"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("default attach argv = %#v, want %#v", got, want)
	}
}

func TestAttachSessionNamedSocketUsesNoStartServer(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"41", "41", ""}}
	tm := &Tmux{cfg: Config{SocketName: "city-socket"}, exec: fe}
	tm.namedSocketLstat = stableNamedSocketLstat(t)
	if err := tm.AttachSession("runner"); err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	path, err := filepath.Abs(filepath.Clean(namedSocketPath("city-socket")))
	if err != nil {
		t.Fatalf("canonical socket path: %v", err)
	}
	want := [][]string{
		{"-u", "-N", "-S", path, "display-message", "-p", "#{pid}"},
		{"-u", "-N", "-S", path, "display-message", "-p", "#{pid}"},
		{"-u", "-N", "-S", path, "attach-session", "-t", "runner"},
	}
	if !reflect.DeepEqual(fe.calls, want) {
		t.Fatalf("tmux calls = %#v, want %#v", fe.calls, want)
	}
}

func TestHiddenAttachNamedSocketUsesWitnessedSocketPath(t *testing.T) {
	tm := &Tmux{cfg: Config{SocketName: "city-socket"}}
	witness := namedSocketWitness{canonicalPath: "/tmp/witnessed-socket"}
	if got, want := tm.hiddenAttachCommandArgsForWitness("runner", witness), []string{"-u", "-N", "-S", "/tmp/witnessed-socket", "attach-session", "-t", "runner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hidden attach argv = %#v, want %#v", got, want)
	}
}

func TestProviderAttachNamedSocketUsesNoStartServer(t *testing.T) {
	binDir := t.TempDir()
	invocations := filepath.Join(binDir, "tmux-invocations")
	fakeTmux := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+invocations+"\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir)

	p := NewProviderWithConfig(Config{SocketName: "city-socket"})
	p.tm.exec = &fakeExecutor{outs: []string{"41", "", "0", "41"}}
	stable := stableNamedSocketObservation(t)
	witnessCalls := 0
	p.tm.namedSocketLstat = func(context.Context, string) (namedSocketObservation, error) {
		witnessCalls++
		if witnessCalls == 1 && len(p.tm.exec.(*fakeExecutor).calls) != 0 {
			t.Fatalf("witness %d observed %d tmux checks, want 0", witnessCalls, len(p.tm.exec.(*fakeExecutor).calls))
		}
		if witnessCalls == 3 && len(p.tm.exec.(*fakeExecutor).calls) != 3 {
			t.Fatalf("witness %d observed %d tmux checks, want %d", witnessCalls, len(p.tm.exec.(*fakeExecutor).calls), 3)
		}
		return stable, nil
	}
	if err := p.Attach("runner"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	path, err := filepath.Abs(filepath.Clean(namedSocketPath("city-socket")))
	if err != nil {
		t.Fatalf("canonical socket path: %v", err)
	}
	wantPreflight := [][]string{
		{"-u", "-N", "-S", path, "display-message", "-p", "#{pid}"},
		{"-u", "-L", "city-socket", "-N", "has-session", "-t", "=runner"},
		{"-u", "-L", "city-socket", "-N", "display-message", "-t", "=runner:^.0", "-p", "#{pane_dead}"},
		{"-u", "-N", "-S", path, "display-message", "-p", "#{pid}"},
	}
	if !reflect.DeepEqual(p.tm.exec.(*fakeExecutor).calls, wantPreflight) {
		t.Fatalf("provider preflight argv = %#v, want %#v", p.tm.exec.(*fakeExecutor).calls, wantPreflight)
	}
	output, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatalf("read fake tmux invocations: %v", err)
	}
	if got, want := string(output), "-u -N -S "+path+" attach-session -t runner\n"; got != want {
		t.Fatalf("provider attach argv = %q, want %q", got, want)
	}
}

func stableNamedSocketNode(t *testing.T) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "socket-node")
	if err := os.WriteFile(path, []byte("node"), 0o600); err != nil {
		t.Fatalf("write witness node: %v", err)
	}
	node, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat witness node: %v", err)
	}
	return node
}

func stableNamedSocketObservation(t *testing.T) namedSocketObservation {
	t.Helper()
	return namedSocketObservation{node: stableNamedSocketNode(t), isSocket: true}
}

func stableNamedSocketLstat(t *testing.T) func(context.Context, string) (namedSocketObservation, error) {
	t.Helper()
	observation := stableNamedSocketObservation(t)
	return func(context.Context, string) (namedSocketObservation, error) { return observation, nil }
}

func TestNamedSocketWitnessRefusesReplacementBeforeEveryAttachLaunch(t *testing.T) {
	originalPath := filepath.Join(t.TempDir(), "original")
	if err := os.WriteFile(originalPath, []byte("original"), 0o600); err != nil {
		t.Fatalf("write original witness: %v", err)
	}
	original, err := os.Lstat(originalPath)
	if err != nil {
		t.Fatalf("lstat original witness: %v", err)
	}
	replacementPath := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("write replacement witness: %v", err)
	}
	replacement, err := os.Lstat(replacementPath)
	if err != nil {
		t.Fatalf("lstat replacement witness: %v", err)
	}
	lstats := func() func(context.Context, string) (namedSocketObservation, error) {
		calls := 0
		return func(context.Context, string) (namedSocketObservation, error) {
			calls++
			if calls <= 2 {
				return namedSocketObservation{node: original, isSocket: true}, nil
			}
			return namedSocketObservation{node: replacement, isSocket: true}, nil
		}
	}

	t.Run("direct", func(t *testing.T) {
		fe := &fakeExecutor{outs: []string{"41", "42"}}
		tm := &Tmux{cfg: Config{SocketName: "city-socket"}, exec: fe, namedSocketLstat: lstats()}
		err := tm.AttachSession("runner")
		if !errors.Is(err, ErrServerDegraded) || len(fe.calls) != 2 {
			t.Fatalf("AttachSession = %v, calls=%v; want degraded refusal before final launch", err, fe.calls)
		}
	})

	t.Run("provider", func(t *testing.T) {
		binDir := t.TempDir()
		invocations := filepath.Join(binDir, "tmux-invocations")
		if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+invocations+"\n"), 0o755); err != nil {
			t.Fatalf("write fake tmux: %v", err)
		}
		t.Setenv("PATH", binDir)
		p := NewProviderWithConfig(Config{SocketName: "city-socket"})
		p.tm.exec = &fakeExecutor{outs: []string{"41", "", "0", "42"}}
		lstatCalls := 0
		p.tm.namedSocketLstat = func(context.Context, string) (namedSocketObservation, error) {
			lstatCalls++
			if lstatCalls == 1 && len(p.tm.exec.(*fakeExecutor).calls) != 0 {
				t.Fatalf("initial lstat ran after tmux checks: %v", p.tm.exec.(*fakeExecutor).calls)
			}
			if lstatCalls == 3 && len(p.tm.exec.(*fakeExecutor).calls) != 3 {
				t.Fatalf("verification lstat ran before provider preflight: %v", p.tm.exec.(*fakeExecutor).calls)
			}
			if lstatCalls <= 2 {
				return namedSocketObservation{node: original, isSocket: true}, nil
			}
			return namedSocketObservation{node: replacement, isSocket: true}, nil
		}
		err := p.Attach("runner")
		if !errors.Is(err, ErrServerDegraded) {
			t.Fatalf("Provider.Attach = %v, want degraded refusal", err)
		}
		if output, readErr := os.ReadFile(invocations); readErr == nil && len(output) != 0 {
			t.Fatalf("Provider.Attach launched replacement: %s", output)
		}
	})

	t.Run("hidden", func(t *testing.T) {
		binDir := t.TempDir()
		started := filepath.Join(binDir, "started")
		if err := os.WriteFile(filepath.Join(binDir, "script"), []byte("#!/bin/sh\n: > "+fmt.Sprintf("%q", started)+"\n"), 0o755); err != nil {
			t.Fatalf("write fake script: %v", err)
		}
		t.Setenv("PATH", binDir)
		fe := &fakeExecutor{outs: []string{"41", "", "42"}}
		lstatCalls := 0
		tm := &Tmux{cfg: Config{SocketName: "city-socket"}, exec: fe}
		tm.namedSocketLstat = func(context.Context, string) (namedSocketObservation, error) {
			lstatCalls++
			if lstatCalls == 1 && len(fe.calls) != 0 {
				t.Fatalf("initial lstat ran after tmux checks: %v", fe.calls)
			}
			if lstatCalls == 3 && len(fe.calls) != 2 {
				t.Fatalf("verification lstat ran before hidden preflight: %v", fe.calls)
			}
			if lstatCalls <= 2 {
				return namedSocketObservation{node: original, isSocket: true}, nil
			}
			return namedSocketObservation{node: replacement, isSocket: true}, nil
		}
		err := tm.ensureHiddenAttachedClient("runner")
		if !errors.Is(err, ErrServerDegraded) {
			t.Fatalf("ensureHiddenAttachedClient = %v, want degraded refusal", err)
		}
		if _, statErr := os.Stat(started); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("hidden attach started replacement script: %v", statErr)
		}
	})
}

func TestProviderAttachRefusesDisappearanceAfterInitialWitness(t *testing.T) {
	originalPath := filepath.Join(t.TempDir(), "original")
	if err := os.WriteFile(originalPath, []byte("original"), 0o600); err != nil {
		t.Fatalf("write original witness: %v", err)
	}
	original, err := os.Lstat(originalPath)
	if err != nil {
		t.Fatalf("lstat original witness: %v", err)
	}
	replacementPath := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("write replacement witness: %v", err)
	}
	replacement, err := os.Lstat(replacementPath)
	if err != nil {
		t.Fatalf("lstat replacement witness: %v", err)
	}

	for _, tc := range []struct {
		name string
	}{
		{name: "missing"},
		{name: "replaced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{}
			if tc.name == "missing" {
				fe.outs = []string{"41", ""}
				fe.errs = []error{nil, ErrNoServer}
			} else {
				fe.outs = []string{"41", "", "42"}
				fe.errs = []error{nil, ErrNoServer, nil}
			}
			p := NewProviderWithConfig(Config{SocketName: "city-socket"})
			p.tm.exec = fe
			lstatCalls := 0
			p.tm.namedSocketLstat = func(context.Context, string) (namedSocketObservation, error) {
				lstatCalls++
				if lstatCalls == 1 {
					if len(fe.calls) != 0 {
						t.Fatalf("initial lstat ran after has-session: %v", fe.calls)
					}
				}
				if lstatCalls <= 2 {
					return namedSocketObservation{node: original, isSocket: true}, nil
				}
				if lstatCalls == 3 && len(fe.calls) != 2 {
					t.Fatalf("verification lstat ran before has-session ErrNoServer: %v", fe.calls)
				}
				if tc.name == "missing" {
					return namedSocketObservation{}, os.ErrNotExist
				}
				return namedSocketObservation{node: replacement, isSocket: true}, nil
			}

			err := p.Attach("missing")
			if !errors.Is(err, ErrServerDegraded) || errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("Attach after has-session ErrNoServer = %v, want degraded without not-found", err)
			}
			wantLstatCalls := 3
			if tc.name == "replaced" {
				wantLstatCalls = 4
			}
			if lstatCalls != wantLstatCalls || len(fe.calls) != 2+(wantLstatCalls-3) {
				t.Fatalf("lstat calls=%d tmux calls=%v, want A, has-session, B", lstatCalls, fe.calls)
			}
		})
	}
}

// The reconciler probes attachment and last activity for every session it
// tracks. Before gcy-8gwi each probe forked its own tmux process —
// `display-message -t <s>` plus `list-windows -t <s>` per session, 2N forks
// per pass on top of the fleet snapshot IsRunning already takes. Both now read
// the snapshot the cache already holds, so a whole pass costs the one
// list-panes call and nothing per session.
func TestProviderAttachmentAndActivityServedFromFleetSnapshot(t *testing.T) {
	fe := &fakeExecutor{
		out: strings.Join([]string{
			"agent-1\t0\tclaude\t101\t0\t1000",
			"agent-2\t0\tclaude\t102\t1\t2000",
			"agent-3\t0\tclaude\t103\t0\t3000",
		}, "\n"),
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	for name, want := range map[string]bool{"agent-1": false, "agent-2": true, "agent-3": false} {
		if got := p.IsAttached(name); got != want {
			t.Errorf("IsAttached(%s) = %t, want %t", name, got, want)
		}
	}
	for name, want := range map[string]int64{"agent-1": 1000, "agent-2": 2000, "agent-3": 3000} {
		got, err := p.GetLastActivity(name)
		if err != nil {
			t.Errorf("GetLastActivity(%s) error = %v", name, err)
			continue
		}
		if !got.Equal(time.Unix(want, 0)) {
			t.Errorf("GetLastActivity(%s) = %v, want %v", name, got, time.Unix(want, 0))
		}
	}

	if len(fe.calls) != 1 {
		t.Fatalf("tmux calls = %d, want 1 (list-panes only); per-session forks are the bug: %v", len(fe.calls), fe.calls)
	}
	if joined := strings.Join(fe.calls[0], " "); !strings.Contains(joined, "list-panes") {
		t.Fatalf("single tmux call = %q, want the list-panes fleet snapshot", joined)
	}
}

// A session absent from the snapshot (created since the last refresh, or a
// target that is not a bare session name) must fall back to the direct
// per-session reads rather than reporting detached-with-no-activity.
func TestProviderAttachmentAndActivityFallBackWhenSnapshotMissesSession(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{
			"agent-1\t0\tclaude\t101\t0\t1000", // list-panes: no agent-2
			"agent-2|1",                        // display-message for agent-2 (name|client-count)
			"5000",                             // list-windows for agent-2
		},
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	if !p.IsAttached("agent-2") {
		t.Error("IsAttached(agent-2) = false, want true from the direct read")
	}
	if len(fe.calls) < 2 || !strings.Contains(strings.Join(fe.calls[1], " "), "display-message") {
		t.Fatalf("tmux calls = %v, want the per-session display-message attachment fallback after list-panes", fe.calls)
	}
	got, err := p.GetLastActivity("agent-2")
	if err != nil {
		t.Fatalf("GetLastActivity(agent-2) error = %v", err)
	}
	if !got.Equal(time.Unix(5000, 0)) {
		t.Fatalf("GetLastActivity(agent-2) = %v, want %v from the direct read", got, time.Unix(5000, 0))
	}

	joined := strings.Join(fe.calls[len(fe.calls)-1], " ")
	if !strings.Contains(joined, "list-windows") {
		t.Fatalf("last tmux call = %q, want the per-session list-windows fallback", joined)
	}
}

// Reading activity from the snapshot must not lose the poke discount: a
// woken-but-unresponsive agent whose only "activity" is gc's own send-keys
// echo still reports its pre-poke activity (#3049). Serving the raw timestamp
// from a batch would make every parked agent look freshly active.
func TestProviderLastActivityFromSnapshotStillDiscountsPoke(t *testing.T) {
	now := time.Now()
	poke := now.Add(-time.Minute)
	prior := now.Add(-time.Hour)
	fe := &fakeExecutor{
		out: fmt.Sprintf("agent-1\t0\tclaude\t101\t0\t%d", poke.Unix()),
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe
	p.tm.recordPokeAt("agent-1", prior, poke)

	got, err := p.GetLastActivity("agent-1")
	if err != nil {
		t.Fatalf("GetLastActivity error = %v", err)
	}
	if got.Unix() != prior.Unix() {
		t.Fatalf("GetLastActivity = %v, want the pre-poke activity %v: the snapshot path must apply the same poke discount as the per-session read", got, prior)
	}
}
