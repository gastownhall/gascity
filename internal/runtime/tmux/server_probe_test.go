package tmux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// probeAssertSet returns a fakeExecutor pre-loaded with one entry per
// scripted tmux call. Indexes into outs/errs advance per call. The first
// entry covers the has-session probe; subsequent entries cover the
// new-session and best-effort set-option calls.
func probeAssertSet(outs []string, errs []error) *fakeExecutor {
	return &fakeExecutor{outs: outs, errs: errs}
}

// firstArgsContainHasSession returns true if any element of args contains
// the probe's has-session verb. Used to disambiguate the probe call from
// new-session calls when checking recorded executor invocations.
func firstArgsContainHasSession(args []string) bool {
	for _, a := range args {
		if a == "has-session" {
			return true
		}
	}
	return false
}

func TestNewSessionErrNoServerRefusesObservedLiveNamedSocket(t *testing.T) {
	variants := []struct {
		name string
		call func(*Tmux) error
	}{
		{name: "NewSession", call: func(tm *Tmux) error {
			return tm.NewSession("gc-live-socket", "")
		}},
		{name: "NewSessionWithCommand", call: func(tm *Tmux) error {
			return tm.NewSessionWithCommand("gc-live-socket", "", "true")
		}},
		{name: "NewSessionWithCommandAndEnv", call: func(tm *Tmux) error {
			return tm.NewSessionWithCommandAndEnv("gc-live-socket", "", "true", map[string]string{"X": "1"})
		}},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			socketName := "gc-live-socket"
			tmuxTmpDir := "/tmux-private"
			t.Setenv("TMUX_TMPDIR", tmuxTmpDir)
			socketPath := filepath.Join(tmuxTmpDir, fmt.Sprintf("tmux-%d", os.Getuid()), socketName)
			observerCalls := 0
			fe := &fakeExecutor{err: ErrNoServer}
			tm := &Tmux{
				cfg:  Config{SocketName: socketName},
				exec: fe,
				serverSocketObserver: func(ctx context.Context, gotPath string) error {
					observerCalls++
					if ctx.Err() != nil {
						t.Fatalf("observer context unexpectedly canceled: %v", ctx.Err())
					}
					if gotPath != socketPath {
						t.Fatalf("observer path = %q, want %q", gotPath, socketPath)
					}
					return fmt.Errorf("live socket path=%s inode=97 peer_pid=4242", gotPath)
				},
			}
			err := variant.call(tm)
			if !errors.Is(err, ErrServerDegraded) {
				t.Fatalf("err = %v, want ErrServerDegraded", err)
			}
			if errors.Is(err, ErrNoServer) {
				t.Fatalf("err = %v, must not wrap ErrNoServer", err)
			}
			for _, want := range []string{
				"protocol=no-server",
				"path=" + socketPath,
				"inode=97",
				"peer_pid=4242",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %q, want %q", err, want)
				}
			}
			if observerCalls != 1 {
				t.Fatalf("observer calls = %d, want 1", observerCalls)
			}
			if len(fe.calls) != 1 || !firstArgsContainHasSession(fe.calls[0]) {
				t.Fatalf("calls = %#v, want exactly the preflight has-session probe", fe.calls)
			}
		})
	}
}

func TestNewSessionErrNoServerObservedSafeAllowsCreation(t *testing.T) {
	for _, observation := range []struct {
		name string
		err  error
	}{
		{name: "absent"},
		{name: "stable-refused"},
	} {
		t.Run(observation.name, func(t *testing.T) {
			fe := probeAssertSet([]string{"", "", ""}, []error{ErrNoServer, nil, nil})
			observerCalls := 0
			tm := &Tmux{
				cfg:  Config{SocketName: "gc-test"},
				exec: fe,
				serverSocketObserver: func(context.Context, string) error {
					observerCalls++
					return observation.err
				},
			}

			if err := tm.NewSession("gc-fresh", ""); err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			if observerCalls != 1 {
				t.Fatalf("observer calls = %d, want 1", observerCalls)
			}
			if len(fe.calls) < 2 || fe.calls[1][3] != "new-session" {
				t.Fatalf("calls = %#v, want probe followed by new-session", fe.calls)
			}
		})
	}
}

func TestNewSessionErrNoServerUnknownObservationFailsClosed(t *testing.T) {
	t.Run("unknown observer", func(t *testing.T) {
		fe := &fakeExecutor{err: ErrNoServer}
		tm := &Tmux{
			cfg:  Config{SocketName: "gc-test"},
			exec: fe,
			serverSocketObserver: func(context.Context, string) error {
				return errors.New("socket observation failed")
			},
		}

		err := tm.NewSession("gc-unknown-observation", "")
		if !errors.Is(err, ErrServerDegraded) {
			t.Fatalf("err = %v, want ErrServerDegraded", err)
		}
		if errors.Is(err, ErrNoServer) {
			t.Fatalf("err = %v, must not wrap ErrNoServer", err)
		}
		if len(fe.calls) != 1 {
			t.Fatalf("calls = %#v, want only the preflight probe", fe.calls)
		}
	})

	socketInfo := func(t *testing.T) os.FileInfo {
		t.Helper()
		path := filepath.Join(t.TempDir(), "socket-fixture")
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatalf("write socket fixture: %v", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("lstat socket fixture: %v", err)
		}
		return socketModeFileInfo{FileInfo: info}
	}
	dialUnexpected := func(context.Context, string) (net.Conn, error) {
		return nil, errors.New("unexpected dial failure")
	}

	t.Run("non-socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "plain-file")
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatalf("write plain fixture: %v", err)
		}
		err := observeNamedSocketWith(context.Background(), path, os.Lstat, dialUnexpected)
		if err == nil || !strings.Contains(err.Error(), "reason=not-unix-socket") {
			t.Fatalf("observe non-socket error = %v, want non-socket refusal", err)
		}
	})

	t.Run("initial permission failure", func(t *testing.T) {
		err := observeNamedSocketWith(context.Background(), "permission-denied", func(string) (os.FileInfo, error) {
			return nil, os.ErrPermission
		}, dialUnexpected)
		if err == nil || !strings.Contains(err.Error(), "lstat=") {
			t.Fatalf("observe permission failure = %v, want lstat refusal", err)
		}
	})

	t.Run("unexpected dial failure", func(t *testing.T) {
		info := socketInfo(t)
		err := observeNamedSocketWith(context.Background(), "unexpected-dial", func(string) (os.FileInfo, error) {
			return info, nil
		}, dialUnexpected)
		if err == nil || !strings.Contains(err.Error(), "unexpected dial failure") {
			t.Fatalf("observe unexpected dial failure = %v, want fail closed", err)
		}
	})

	t.Run("dial cancellation fails closed", func(t *testing.T) {
		info := socketInfo(t)
		for _, dialErr := range []error{context.Canceled, context.DeadlineExceeded} {
			err := observeNamedSocketWith(context.Background(), "dial-canceled", func(string) (os.FileInfo, error) {
				return info, nil
			}, func(context.Context, string) (net.Conn, error) {
				return nil, dialErr
			})
			if err == nil || !strings.Contains(err.Error(), dialErr.Error()) {
				t.Fatalf("observe dial %v = %v, want fail closed", dialErr, err)
			}
		}
	})

	t.Run("post-lstat identity replacement", func(t *testing.T) {
		first := socketInfo(t)
		second := socketInfo(t)
		calls := 0
		err := observeNamedSocketWith(context.Background(), "identity-replaced", func(string) (os.FileInfo, error) {
			calls++
			if calls == 1 {
				return first, nil
			}
			return second, nil
		}, func(context.Context, string) (net.Conn, error) {
			return nil, syscall.ECONNREFUSED
		})
		if err == nil || !strings.Contains(err.Error(), "socket-identity-changed") {
			t.Fatalf("observe identity replacement = %v, want fail closed", err)
		}
	})

	t.Run("already canceled context skips lstat", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		err := observeNamedSocketWith(ctx, "canceled-before-lstat", func(string) (os.FileInfo, error) {
			called = true
			return nil, nil
		}, dialUnexpected)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("observe canceled context = %v, want context canceled", err)
		}
		if called {
			t.Fatal("lstat ran after context cancellation")
		}
	})

	t.Run("blocking lstat returns on cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered := make(chan struct{})
		release := make(chan struct{})
		finished := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			result <- observeNamedSocketWith(ctx, "blocking-lstat", func(string) (os.FileInfo, error) {
				close(entered)
				<-release
				close(finished)
				return nil, os.ErrNotExist
			}, dialUnexpected)
		}()
		<-entered
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("observe canceled blocking lstat = %v, want context canceled", err)
		}
		close(release)
		<-finished
	})
}

type socketModeFileInfo struct{ os.FileInfo }

func (info socketModeFileInfo) Mode() os.FileMode { return info.FileInfo.Mode() | os.ModeSocket }

func TestProbeServerAliveHealthyProtocolDoesNotObserveSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "session-not-found", err: ErrSessionNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observerCalls := 0
			tm := &Tmux{
				cfg:  Config{SocketName: "gc-test"},
				exec: &fakeExecutor{err: tc.err},
				serverSocketObserver: func(context.Context, string) error {
					observerCalls++
					return errors.New("observer must not run")
				},
			}

			if err := tm.probeServerAlive(); err != nil {
				t.Fatalf("probeServerAlive: %v", err)
			}
			if observerCalls != 0 {
				t.Fatalf("observer calls = %d, want 0", observerCalls)
			}
		})
	}
}

func TestProbeServerAliveUnknownProtocolDoesNotObserveSocket(t *testing.T) {
	observerCalls := 0
	tm := &Tmux{
		cfg:  Config{SocketName: "gc-test"},
		exec: &fakeExecutor{err: errors.New("tmux protocol failure")},
		serverSocketObserver: func(context.Context, string) error {
			observerCalls++
			return nil
		},
	}

	err := tm.probeServerAlive()
	if !errors.Is(err, ErrServerDegraded) {
		t.Fatalf("probeServerAlive error = %v, want ErrServerDegraded", err)
	}
	if observerCalls != 0 {
		t.Fatalf("observer calls = %d, want 0", observerCalls)
	}
}

func TestNamedSocketPathUsesTMUXTMPDIRAndIgnoresTMPDIR(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "/tmux-private")
	t.Setenv("TMPDIR", "/must-not-be-used")
	if got, want := namedSocketPath("gc-test"), filepath.Join("/tmux-private", fmt.Sprintf("tmux-%d", os.Getuid()), "gc-test"); got != want {
		t.Fatalf("namedSocketPath() = %q, want %q", got, want)
	}
}

func TestNamedSocketPathFallsBackToTmpWhenTMUXTMPDIREmpty(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "")
	t.Setenv("TMPDIR", "/must-not-be-used")
	if got, want := namedSocketPath("gc-test"), filepath.Join("/tmp", fmt.Sprintf("tmux-%d", os.Getuid()), "gc-test"); got != want {
		t.Fatalf("namedSocketPath() = %q, want %q", got, want)
	}
}

func TestNewSessionSkipsProbeWhenSocketEmpty(t *testing.T) {
	fe := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = fe

	if err := tm.NewSession("gc-no-socket", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if len(fe.calls) == 0 {
		t.Fatal("expected at least one tmux call")
	}
	if firstArgsContainHasSession(fe.calls[0]) {
		t.Fatalf("probe ran with empty SocketName: %v", fe.calls[0])
	}
}

func TestNewSessionProbesBeforeCreatingWhenSocketSet(t *testing.T) {
	fe := probeAssertSet(
		[]string{"", "", ""},
		[]error{ErrSessionNotFound, nil, nil},
	)
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}

	if err := tm.NewSession("gc-probe-ok", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if len(fe.calls) < 2 {
		t.Fatalf("expected probe + new-session, got %d calls: %v", len(fe.calls), fe.calls)
	}
	probe := fe.calls[0]
	want := []string{"-u", "-L", "gc-test", "has-session", "-t", "=" + probeSessionName}
	if len(probe) != len(want) {
		t.Fatalf("probe args = %v, want %v", probe, want)
	}
	for i := range want {
		if probe[i] != want[i] {
			t.Errorf("probe arg %d = %q, want %q", i, probe[i], want[i])
		}
	}
	create := fe.calls[1]
	if create[3] != "new-session" {
		t.Fatalf("second call should be new-session, got %v", create)
	}
}

func TestNewSessionProceedsWhenProbeReportsNoServer(t *testing.T) {
	fe := probeAssertSet(
		[]string{"", "", ""},
		[]error{ErrNoServer, nil, nil},
	)
	tm := &Tmux{
		cfg:  Config{SocketName: "gc-test"},
		exec: fe,
		serverSocketObserver: func(context.Context, string) error {
			return nil
		},
	}

	if err := tm.NewSession("gc-fresh", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if len(fe.calls) < 2 {
		t.Fatalf("expected new-session to follow probe, got %d calls: %v", len(fe.calls), fe.calls)
	}
	if fe.calls[1][3] != "new-session" {
		t.Fatalf("expected new-session after no-server probe, got %v", fe.calls[1])
	}
}

func TestNewSessionBailsWhenProbeReportsDegradedServer(t *testing.T) {
	degraded := errors.New("tmux has-session: connection refused")
	fe := probeAssertSet(
		[]string{""},
		[]error{degraded},
	)
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}

	err := tm.NewSession("gc-bail", "")
	if err == nil {
		t.Fatal("NewSession returned nil, want ErrServerDegraded")
	}
	if !errors.Is(err, ErrServerDegraded) {
		t.Fatalf("err = %v, want ErrServerDegraded", err)
	}
	if !strings.Contains(err.Error(), "gc-test") {
		t.Errorf("err = %q, want error mentioning socket name gc-test", err.Error())
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected only probe call, got %d: %v", len(fe.calls), fe.calls)
	}
}

func TestNewSessionWithCommandBailsWhenProbeDegraded(t *testing.T) {
	fe := probeAssertSet(
		[]string{""},
		[]error{errors.New("tmux: lost server")},
	)
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}

	err := tm.NewSessionWithCommand("gc-cmd-bail", "", "claude")
	if !errors.Is(err, ErrServerDegraded) {
		t.Fatalf("err = %v, want ErrServerDegraded", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected only probe call, got %d: %v", len(fe.calls), fe.calls)
	}
}

func TestNewSessionWithCommandAndEnvBailsWhenProbeDegraded(t *testing.T) {
	fe := probeAssertSet(
		[]string{""},
		[]error{errors.New("tmux: unknown failure")},
	)
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}

	err := tm.NewSessionWithCommandAndEnv("gc-env-bail", "", "claude", map[string]string{"X": "1"})
	if !errors.Is(err, ErrServerDegraded) {
		t.Fatalf("err = %v, want ErrServerDegraded", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected only probe call, got %d: %v", len(fe.calls), fe.calls)
	}
}

func TestProbeServerAliveAcceptsHealthyServer(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "ErrSessionNotFound", err: ErrSessionNotFound},
		{name: "nil", err: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{err: tc.err}
			tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}
			if err := tm.probeServerAlive(); err != nil {
				t.Fatalf("probeServerAlive(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}

// slowExecutor blocks each execute call until the supplied context is
// canceled, then returns ctx.Err(). Used to verify the probe respects its
// short timeout and does not inherit the 30s tmuxSubprocessTimeout cap.
type slowExecutor struct {
	calls [][]string
}

func (s *slowExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	s.calls = append(s.calls, cp)
	time.Sleep(10 * time.Second)
	return "", fmt.Errorf("slowExecutor: not invoked via executeCtx")
}

func (s *slowExecutor) executeCtx(ctx context.Context, args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	s.calls = append(s.calls, cp)
	<-ctx.Done()
	return "", ctx.Err()
}

func TestProbeServerAliveBailsFastOnHang(t *testing.T) {
	prev := newSessionProbeTimeout
	newSessionProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { newSessionProbeTimeout = prev })

	se := &slowExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: se}

	start := time.Now()
	err := tm.probeServerAlive()
	elapsed := time.Since(start)

	if !errors.Is(err, ErrServerDegraded) {
		t.Fatalf("err = %v, want ErrServerDegraded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("probe took %s, want < 2s (timeout should be ~100ms)", elapsed)
	}
}
