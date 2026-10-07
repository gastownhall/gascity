package tmux

import (
	"errors"
	"testing"

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
