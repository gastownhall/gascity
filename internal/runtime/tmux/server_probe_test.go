package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}

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
		{name: "ErrNoServer", err: ErrNoServer},
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
