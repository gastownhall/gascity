package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestConfigureServerSendsSetOptionExitEmptyOff verifies that ConfigureServer
// issues set-option -g exit-empty off through the executor.
func TestConfigureServerSendsSetOptionExitEmptyOff(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	if err := tm.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer() error = %v", err)
	}

	for _, call := range fe.calls {
		if containsSetOptionExitEmptyWithValue(call, "off") {
			return
		}
	}
	t.Fatalf("ConfigureServer did not issue set-option -g exit-empty off; calls = %v", fe.calls)
}

// TestConfigureServerReappliesExitEmptyForReplacementServer verifies that
// server configuration is applied on every call. A Tmux wrapper can outlive
// the server bound to its socket; per-instance sync.Once would leave a
// replacement server at tmux's unsafe exit-empty=on default.
func TestConfigureServerReappliesExitEmptyForReplacementServer(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	for i := range 2 {
		if err := tm.ConfigureServer(); err != nil {
			t.Fatalf("ConfigureServer() call %d error = %v", i, err)
		}
	}

	count := 0
	for _, call := range fe.calls {
		if containsSetOptionExitEmptyWithValue(call, "off") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("set-option -g exit-empty off issued %d times across 2 ConfigureServer calls, want 2", count)
	}
}

func TestConfigureServerRetriesExitEmptyAfterFailure(t *testing.T) {
	firstAttempt := errors.New("set option unavailable")
	fe := &fakeExecutor{errs: []error{firstAttempt, nil}}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	if err := tm.ConfigureServer(); !errors.Is(err, firstAttempt) {
		t.Fatalf("first ConfigureServer() error = %v, want %v", err, firstAttempt)
	}
	if err := tm.ConfigureServer(); err != nil {
		t.Fatalf("second ConfigureServer() error = %v, want nil after retry", err)
	}

	count := 0
	for _, call := range fe.calls {
		if containsSetOptionExitEmptyWithValue(call, "off") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("set-option -g exit-empty off issued %d times across failed retry, want 2", count)
	}
}

// TestProviderStopDistinguishesMissingSessionFromMissingServer pins the Stop
// contract: a responsive server's missing-session answer is idempotent
// success, while a missing server reaches the caller. Teardown callers still
// absorb every row through runtime.StopForCleanup, which accepts an error only
// when each leaf is a gone answer — so this also pins that the real
// missing-server shape (pane capture and kill-session both failing) carries
// nothing but ErrNoServer leaves.
func TestProviderStopDistinguishesMissingSessionFromMissingServer(t *testing.T) {
	tests := []struct {
		name     string
		stopErr  error
		stopErrs []error
		wantErr  error
	}{
		{
			name:    "responsive server missing session is idempotent",
			stopErr: ErrSessionNotFound,
		},
		{
			name:    "responsive empty server is idempotent",
			stopErr: ErrNoCurrentTarget,
		},
		{
			name:    "missing server remains uncertain",
			stopErr: ErrNoServer,
			wantErr: ErrNoServer,
		},
		{
			name:     "missing server at kill outranks an earlier missing pane",
			stopErrs: []error{ErrSessionNotFound, ErrNoServer},
			wantErr:  ErrNoServer,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Each Stop gets its own executor: a sequenced fake is spent by
			// one Stop, so a shared one would answer the cleanup leg's pane
			// capture with empty output instead of replaying the row.
			newProvider := func() *Provider {
				provider := NewProviderWithConfig(Config{SocketName: "gctest-stop-outcome"})
				provider.tm.exec = &fakeExecutor{err: test.stopErr, errs: test.stopErrs}
				return provider
			}

			err := newProvider().Stop("worker")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Stop() error = %v, want %v", err, test.wantErr)
			}
			if err := runtime.StopForCleanup(newProvider(), "worker"); err != nil {
				t.Fatalf("runtime.StopForCleanup() error = %v, want nil", err)
			}
		})
	}
}

// TestTeardownServerCallsKillServer verifies that TeardownServer delegates to
// tmux kill-server via the executor.
func TestTeardownServerCallsKillServer(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	if err := tm.TeardownServer(); err != nil {
		t.Fatalf("TeardownServer() error = %v", err)
	}

	for _, call := range fe.calls {
		for _, arg := range call {
			if arg == "kill-server" {
				return
			}
		}
	}
	t.Fatalf("TeardownServer did not call kill-server; calls = %v", fe.calls)
}

// TestTeardownServerTreatsAlreadyGoneServerAsSuccess verifies that TeardownServer
// returns nil when the tmux server is already gone (ErrNoServer), consistent with
// KillServer's existing semantics.
func TestTeardownServerTreatsAlreadyGoneServerAsSuccess(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	if err := tm.TeardownServer(); err != nil {
		t.Fatalf("TeardownServer() = %v, want nil for already-gone server", err)
	}
}

// containsSetOptionExitEmptyWithValue returns true if args contains the
// sequence "set-option -g exit-empty <value>", possibly preceded by socket flags.
func containsSetOptionExitEmptyWithValue(args []string, value string) bool {
	for i, arg := range args {
		if arg == "set-option" && i+3 < len(args) &&
			args[i+1] == "-g" && args[i+2] == "exit-empty" && args[i+3] == value {
			return true
		}
	}
	return false
}

// TestKillServerWaitsForListenerToClose verifies that KillServer and
// TeardownServer do not return while the named server socket still answers
// after tmux acknowledged kill-server: an immediate restart would otherwise
// be refused by the new-session preflight with ErrServerDegraded
// (reason=live-unix-socket).
func TestKillServerWaitsForListenerToClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		kill func(tmux *Tmux) error
	}{
		{name: "KillServer", kill: func(tmux *Tmux) error { return tmux.KillServer() }},
		{name: "TeardownServer", kill: func(tmux *Tmux) error { return tmux.TeardownServer() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{}
			const liveObservations = 2
			var paths []string
			var callsAtObservation []int
			tmux := &Tmux{
				cfg:  Config{SocketName: "gc-settle-wait"},
				exec: fe,
				serverSocketObserver: func(_ context.Context, path string) error {
					paths = append(paths, path)
					callsAtObservation = append(callsAtObservation, len(fe.calls))
					if len(paths) <= liveObservations {
						return errors.New("path=" + path + " inode=1 peer_pid=42 reason=live-unix-socket")
					}
					return nil
				},
			}

			if err := tc.kill(tmux); err != nil {
				t.Fatalf("%s() error = %v, want nil after the listener closes", tc.name, err)
			}
			if len(paths) != liveObservations+1 {
				t.Fatalf("%s() made %d socket observations, want %d (the two live ones, then the closed socket)", tc.name, len(paths), liveObservations+1)
			}
			wantPath := tmux.namedServerSocketPath()
			for i, path := range paths {
				if path != wantPath {
					t.Fatalf("observation %d watched %q, want the named server socket path %q", i+1, path, wantPath)
				}
			}
			for i, calls := range callsAtObservation {
				if calls != 1 || !slices.Contains(fe.calls[0], "kill-server") {
					t.Fatalf("observation %d happened with %d executor call(s) %v, want only kill-server", i+1, calls, fe.calls)
				}
			}
		})
	}
}

// TestKillServerReportsListenerThatStaysLive verifies the settle bound: when
// the named socket never stops answering, KillServer returns no sooner than
// killServerSettleTimeout with an ErrServerDegraded error naming the settle
// protocol, the socket path and the last observation, and never wrapping
// ErrNoServer. An observer that blocks until its context ends must not push
// the return past the bound either.
func TestKillServerReportsListenerThatStaysLive(t *testing.T) {
	const bound = 50 * time.Millisecond
	original := killServerSettleTimeout
	killServerSettleTimeout = bound
	t.Cleanup(func() { killServerSettleTimeout = original })

	live := errors.New("inode=7 peer_pid=42 reason=live-unix-socket")
	for _, tc := range []struct {
		name     string
		observer func(context.Context, string) error
	}{
		{
			name: "polling observer",
			observer: func(context.Context, string) error {
				return live
			},
		},
		{
			name: "observer blocked until the deadline",
			observer: func(ctx context.Context, _ string) error {
				<-ctx.Done()
				return live
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{}
			observations := 0
			tmux := &Tmux{
				cfg:  Config{SocketName: "gc-settle-live"},
				exec: fe,
				serverSocketObserver: func(ctx context.Context, path string) error {
					observations++
					return tc.observer(ctx, path)
				},
			}

			start := time.Now()
			err := tmux.KillServer()
			elapsed := time.Since(start)

			if !errors.Is(err, ErrServerDegraded) {
				t.Fatalf("KillServer() error = %v, want ErrServerDegraded", err)
			}
			if errors.Is(err, ErrNoServer) {
				t.Fatalf("KillServer() error = %v, must not wrap ErrNoServer", err)
			}
			for _, want := range []string{"protocol=kill-server-settle", tmux.namedServerSocketPath(), live.Error()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("KillServer() error %q does not contain %q", err, want)
				}
			}
			if elapsed < bound {
				t.Errorf("KillServer() returned after %s, before the %s bound", elapsed, bound)
			}
			if tc.name == "polling observer" && observations < 2 {
				t.Errorf("KillServer() made %d socket observations, want at least 2", observations)
			}
		})
	}
}
