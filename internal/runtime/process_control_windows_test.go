//go:build windows

package runtime

import (
	"errors"
	"os/exec"
	"testing"
	"time"
)

// startSleeper starts a real child that outlives any grace these tests set.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(inBoxPowerShell(), "-NoProfile", "-NonInteractive", "-Command", "Start-Sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	return cmd
}

func TestTerminateManagedProcessWindowsDoneBeforeGraceIsSuccess(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if err := terminateManagedProcessGraces(nil, done, time.Second, time.Second); err != nil {
		t.Fatalf("terminate with done already closed = %v, want nil", err)
	}
}

// A stop that kills only the direct process leaves its descendants running, so
// it must be reported as not confirmed dead: callers check only that sentinel.
func TestTerminateManagedProcessWindowsReportsUnreapedDescendants(t *testing.T) {
	cmd := startSleeper(t)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	err := terminateManagedProcessGraces(cmd, done, 10*time.Millisecond, 5*time.Second)
	if !errors.Is(err, ErrProcessNotConfirmedDead) {
		t.Fatalf("terminate error = %v, want ErrProcessNotConfirmedDead", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped after the kill")
	}
}

// The wait after the kill is bounded: a done that never closes yields the
// sentinel within the reap grace instead of blocking the caller forever.
func TestTerminateManagedProcessWindowsBoundsTheReapWait(t *testing.T) {
	cmd := startSleeper(t)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	never := make(chan struct{})

	res := make(chan error, 1)
	start := time.Now()
	go func() { res <- terminateManagedProcessGraces(cmd, never, 10*time.Millisecond, 50*time.Millisecond) }()
	select {
	case err := <-res:
		if !errors.Is(err, ErrProcessNotConfirmedDead) {
			t.Fatalf("terminate error = %v, want ErrProcessNotConfirmedDead", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("terminate returned after %v, want about the 50ms reap grace", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminate still blocked after 5s: the post-kill wait is unbounded")
	}
}
