package main

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// stubManagedDoltInitWait replaces the injectable functions used by
// waitForManagedDoltInitReady for the duration of a test.
type stubManagedDoltInitWait struct {
	isManaged bool
	state     doltRuntimeState
	stateOK   bool
	reachable bool
	pidAlive  bool
}

func (s *stubManagedDoltInitWait) install(t *testing.T) {
	t.Helper()
	prevIsManaged := managedDoltInitWaitIsManaged
	prevReadState := managedDoltInitWaitReadState
	prevReachable := managedDoltInitWaitReachable
	prevPidAlive := managedDoltInitWaitPidAlive

	managedDoltInitWaitIsManaged = func(_ string) bool { return s.isManaged }
	managedDoltInitWaitReadState = func(_ string) (doltRuntimeState, bool) { return s.state, s.stateOK }
	managedDoltInitWaitReachable = func(_, _ string) bool { return s.reachable }
	managedDoltInitWaitPidAlive = func(_ int) bool { return s.pidAlive }

	t.Cleanup(func() {
		managedDoltInitWaitIsManaged = prevIsManaged
		managedDoltInitWaitReadState = prevReadState
		managedDoltInitWaitReachable = prevReachable
		managedDoltInitWaitPidAlive = prevPidAlive
	})
}

// --- waitForManagedDoltInitReady tests ---

// Criterion: non-managed Dolt city — skip immediately, return nil.
func TestWaitForManagedDoltInitReady_NonManaged(t *testing.T) {
	stub := &stubManagedDoltInitWait{isManaged: false}
	stub.install(t)
	if err := waitForManagedDoltInitReady("any", 5*time.Second); err != nil {
		t.Errorf("non-managed: want nil, got %v", err)
	}
}

// Criterion: fast ready — TCP-reachable immediately, return nil.
func TestWaitForManagedDoltInitReady_FastReady(t *testing.T) {
	stub := &stubManagedDoltInitWait{
		isManaged: true,
		state:     doltRuntimeState{PID: 999, Port: 28231},
		stateOK:   true,
		reachable: true,
		pidAlive:  true,
	}
	stub.install(t)
	if err := waitForManagedDoltInitReady("any", 5*time.Second); err != nil {
		t.Errorf("fast ready: want nil, got %v", err)
	}
}

// Criterion: delayed ready — state/TCP not available at first, then appears.
func TestWaitForManagedDoltInitReady_DelayedReady(t *testing.T) {
	var calls atomic.Int32
	prevIsManaged := managedDoltInitWaitIsManaged
	prevReadState := managedDoltInitWaitReadState
	prevReachable := managedDoltInitWaitReachable
	prevPidAlive := managedDoltInitWaitPidAlive
	t.Cleanup(func() {
		managedDoltInitWaitIsManaged = prevIsManaged
		managedDoltInitWaitReadState = prevReadState
		managedDoltInitWaitReachable = prevReachable
		managedDoltInitWaitPidAlive = prevPidAlive
	})
	managedDoltInitWaitIsManaged = func(_ string) bool { return true }
	managedDoltInitWaitPidAlive = func(_ int) bool { return true }
	managedDoltInitWaitReadState = func(_ string) (doltRuntimeState, bool) {
		n := calls.Add(1)
		if n < 3 {
			return doltRuntimeState{}, false // state not ready yet
		}
		return doltRuntimeState{PID: 123, Port: 28231}, true
	}
	managedDoltInitWaitReachable = func(_, _ string) bool {
		return calls.Load() >= 3
	}

	if err := waitForManagedDoltInitReady("any", 2*time.Second); err != nil {
		t.Errorf("delayed ready: want nil, got %v", err)
	}
	if n := calls.Load(); n < 3 {
		t.Errorf("expected at least 3 poll calls before ready, got %d", n)
	}
}

// Criterion: missing port (state never published within timeout) — return error.
func TestWaitForManagedDoltInitReady_MissingState_Timeout(t *testing.T) {
	stub := &stubManagedDoltInitWait{
		isManaged: true,
		stateOK:   false, // port never published
		reachable: false,
		pidAlive:  true,
	}
	stub.install(t)

	start := time.Now()
	err := waitForManagedDoltInitReady("any", 80*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Error("expected error when state never published, got nil")
	}
	// Must have waited at least most of the timeout.
	if elapsed < 50*time.Millisecond {
		t.Errorf("returned too fast: %v (want ≥50ms)", elapsed)
	}
}

// Criterion: process exits before TCP-ready — return error quickly.
func TestWaitForManagedDoltInitReady_ProcessExits(t *testing.T) {
	stub := &stubManagedDoltInitWait{
		isManaged: true,
		state:     doltRuntimeState{PID: 9999999, Port: 28231},
		stateOK:   true,
		reachable: false, // TCP not ready
		pidAlive:  false, // process exited
	}
	stub.install(t)

	start := time.Now()
	err := waitForManagedDoltInitReady("any", 5*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Error("expected error when process exits, got nil")
	}
	// Should return quickly, not wait the full 5s.
	if elapsed > 500*time.Millisecond {
		t.Errorf("process-exit detection too slow: %v (want <500ms)", elapsed)
	}
}

// Criterion: timeout cleanup — port published and process alive but TCP never ready.
func TestWaitForManagedDoltInitReady_Timeout(t *testing.T) {
	stub := &stubManagedDoltInitWait{
		isManaged: true,
		state:     doltRuntimeState{PID: 123, Port: 28231},
		stateOK:   true,
		reachable: false, // TCP never becomes ready
		pidAlive:  true,
	}
	stub.install(t)

	start := time.Now()
	err := waitForManagedDoltInitReady("any", 80*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
	if elapsed < 50*time.Millisecond {
		t.Errorf("returned too fast: %v (want ≥50ms block)", elapsed)
	}
}

// Verify that managedDoltTCPReachable works correctly with a real socket.
// This is an implementation sanity-check, not a wait test per se.
func TestManagedDoltTCPReachable_RealSocket(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	addr := lis.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if !managedDoltTCPReachable(host, port) {
		t.Errorf("managedDoltTCPReachable(%q, %q) = false, want true", host, port)
	}
	if managedDoltTCPReachable(host, "1") {
		t.Error("managedDoltTCPReachable on port 1 (no listener) = true, want false")
	}
}

// stubManagedDoltDial replaces the dial behind managedDoltTCPProbe for the
// rest of the test.
func stubManagedDoltDial(t *testing.T, dial func(network, address string, timeout time.Duration) (net.Conn, error)) {
	t.Helper()
	prev := managedDoltDialFn
	managedDoltDialFn = dial
	t.Cleanup(func() { managedDoltDialFn = prev })
}

// dialErrorFor builds the error net.Dial returns when the named syscall fails.
func dialErrorFor(syscallName string, errno syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError(syscallName, errno)}
}

func TestManagedDoltDialFailedLocally(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no ephemeral port left", err: dialErrorFor("connect", syscall.EADDRNOTAVAIL), want: true},
		{name: "no buffer space", err: dialErrorFor("connect", syscall.ENOBUFS), want: true},
		{name: "process out of descriptors", err: dialErrorFor("socket", syscall.EMFILE), want: true},
		{name: "host out of descriptors", err: dialErrorFor("socket", syscall.ENFILE), want: true},
		{name: "connection refused", err: dialErrorFor("connect", syscall.ECONNREFUSED), want: false},
		{name: "dial timeout", err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, want: false},
		{name: "no error", err: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltDialFailedLocally(tc.err); got != tc.want {
				t.Fatalf("managedDoltDialFailedLocally(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A one-nanosecond timeout allows exactly one probe before the wait gives up.
func TestWaitForManagedDoltReadyIsInconclusiveWhenEveryDialFailsLocally(t *testing.T) {
	stubManagedDoltDial(t, func(string, string, time.Duration) (net.Conn, error) {
		return nil, dialErrorFor("connect", syscall.EADDRNOTAVAIL)
	})

	report, err := waitForManagedDoltReady(t.TempDir(), "127.0.0.1", "3307", "root", os.Getpid(), time.Nanosecond, false)
	if err == nil {
		t.Fatal("waitForManagedDoltReady() error = nil, want an error for a wait that never reached the server")
	}
	if !errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Fatalf("waitForManagedDoltReady() error = %v, want it to carry the local dial error", err)
	}
	if report.Ready || !report.Inconclusive || !report.PIDAlive {
		t.Fatalf("waitForManagedDoltReady() = %+v, want inconclusive, not ready, pid alive", report)
	}
}

func TestWaitForManagedDoltReadyIsConclusiveWhenTheServerRefuses(t *testing.T) {
	stubManagedDoltDial(t, func(string, string, time.Duration) (net.Conn, error) {
		return nil, dialErrorFor("connect", syscall.ECONNREFUSED)
	})

	report, err := waitForManagedDoltReady(t.TempDir(), "127.0.0.1", "3307", "root", os.Getpid(), time.Nanosecond, false)
	if err == nil {
		t.Fatal("waitForManagedDoltReady() error = nil, want a timeout")
	}
	if report.Ready || report.Inconclusive {
		t.Fatalf("waitForManagedDoltReady() = %+v, want not ready and not inconclusive", report)
	}
}

// TestWaitForManagedDoltReadyCountsARefusalAgainstTheServer pins that one
// refused dial outweighs earlier local failures: a refusal means the dial
// reached the host and nothing was listening there.
func TestWaitForManagedDoltReadyCountsARefusalAgainstTheServer(t *testing.T) {
	var dials atomic.Int32
	stubManagedDoltDial(t, func(string, string, time.Duration) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, dialErrorFor("connect", syscall.EADDRNOTAVAIL)
		}
		return nil, dialErrorFor("connect", syscall.ECONNREFUSED)
	})

	report, err := waitForManagedDoltReady(t.TempDir(), "127.0.0.1", "3307", "root", os.Getpid(), 600*time.Millisecond, false)
	if err == nil {
		t.Fatal("waitForManagedDoltReady() error = nil, want a timeout")
	}
	if n := dials.Load(); n < 2 {
		t.Fatalf("dials = %d, want at least 2 so a refusal follows the local failure", n)
	}
	if report.Inconclusive {
		t.Fatalf("waitForManagedDoltReady() = %+v, want conclusive once a dial was refused", report)
	}
}
