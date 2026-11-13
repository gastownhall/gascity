//go:build (integration || dolt_integration) && unix

package dolt_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// fakeTestDeadline answers Deadline() with a fixed value, so the bound a dolt
// CLI call gets can be asserted without waiting on a real test deadline.
type fakeTestDeadline struct {
	at time.Time
	ok bool
}

func (f fakeTestDeadline) Deadline() (time.Time, bool) { return f.at, f.ok }

// A dolt CLI call is bounded by what is left of the test's own deadline, never
// by a fixed per-call budget: under suite load a merely slow dolt call outlasts
// any fixed budget and is SIGKILLed, which fails its test with "signal: killed".
func TestDoltCallContextIsBoundedByTheTestDeadline(t *testing.T) {
	t.Run("a deadline inside the cap leaves the call everything but the margin", func(t *testing.T) {
		deadline := time.Now().Add(4 * time.Minute)
		ctx, cancel := doltCallContext(fakeTestDeadline{at: deadline, ok: true})
		defer cancel()

		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("call context has no deadline, want the test deadline less doltCallMargin")
		}
		if want := deadline.Add(-doltCallMargin); !got.Equal(want) {
			t.Fatalf("call deadline = %v (%v from now), want %v (test deadline %v less doltCallMargin %v): a fixed per-call budget kills a slow dolt call under suite load",
				got, time.Until(got).Round(time.Second), want, deadline, doltCallMargin)
		}
	})

	t.Run("a far deadline is capped at about five minutes", func(t *testing.T) {
		ctx, cancel := doltCallContext(fakeTestDeadline{at: time.Now().Add(30 * time.Minute), ok: true})
		defer cancel()

		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("call context has no deadline, want the five minute cap")
		}
		// 5m is the cap; the slack on either side absorbs scheduling noise.
		if left := time.Until(got); left > 5*time.Minute+5*time.Second || left < 4*time.Minute {
			t.Fatalf("call deadline is %v from now, want about 5m: a hung dolt holds the test package for its whole timeout less doltCallMargin when the bound is not capped", left.Round(time.Second))
		}
	})

	t.Run("a short deadline keeps up to half for the test to report", func(t *testing.T) {
		deadline := time.Now().Add(20 * time.Second)
		ctx, cancel := doltCallContext(fakeTestDeadline{at: deadline, ok: true})
		defer cancel()

		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("call context has no deadline, want the test deadline less a share of what is left")
		}
		if !got.Before(deadline) {
			t.Fatalf("call deadline %v is not before the test deadline %v: the call would outlive the time the test needs to report it", got, deadline)
		}
		if held := deadline.Sub(got); held > 10*time.Second {
			t.Fatalf("call holds back %v of a 20s deadline, want at most half so the call keeps most of it", held)
		}
		if time.Until(got) <= 0 {
			t.Fatalf("call deadline %v has already passed: the call has no time at all", got)
		}
	})

	t.Run("a deadline that has already passed leaves the call no time", func(t *testing.T) {
		deadline := time.Now().Add(-time.Minute)
		ctx, cancel := doltCallContext(fakeTestDeadline{at: deadline, ok: true})
		defer cancel()

		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("call context has no deadline, want the test deadline that has already passed")
		}
		if got.After(deadline) {
			t.Fatalf("call deadline %v is after the test deadline %v that has already passed: the call would outlive the test", got, deadline)
		}
	})

	t.Run("no test deadline leaves the call unbounded", func(t *testing.T) {
		ctx, cancel := doltCallContext(fakeTestDeadline{})
		defer cancel()

		if got, ok := ctx.Deadline(); ok {
			t.Fatalf("call deadline = %v, want none when the test has no deadline (go test -timeout 0)", got)
		}
	})
}

// A hung dolt is killed at doltCallCap even when the package deadline is far
// away, so one hung call cannot hold a test package for up to its timeout less
// doltCallMargin. The cap is shortened for the test; the far deadline is the
// case where the cap, and not the test deadline, is what must end the call.
func TestDoltCallContextKillsAHungDoltAtTheCap(t *testing.T) {
	prev := doltCallCap
	doltCallCap = 300 * time.Millisecond
	t.Cleanup(func() { doltCallCap = prev })

	// exec, so the kill lands on the sleep itself and no child keeps the pipe.
	fakeDolt := filepath.Join(t.TempDir(), "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}

	ctx, cancel := doltCallContext(fakeTestDeadline{at: time.Now().Add(30 * time.Minute), ok: true})
	defer cancel()
	cmd := exec.CommandContext(ctx, fakeDolt, "commit")
	cmd.WaitDelay = doltCallWaitDelay

	start := time.Now()
	_, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("hung fake dolt exited cleanly after %v, want it killed at the %v cap", elapsed, doltCallCap)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("call context err = %v, want DeadlineExceeded: the call was not ended by its bound (cmd err %v)", ctx.Err(), err)
	}
	if elapsed > testutil.ExecRaceTimeout {
		t.Fatalf("hung fake dolt ran %v, want it killed near the %v cap: a far package deadline leaves a hung call unbounded", elapsed, doltCallCap)
	}
}

// A compact run whose script exits while a child it started still holds the
// output pipe must return once doltCallWaitDelay has passed instead of blocking
// until that child exits too. Without a delay, a dolt left running by run.sh
// pins the helper until the package timeout.
func TestCompactScriptRunnerDoesNotBlockOnPipeHeldByLeakedChild(t *testing.T) {
	root := t.TempDir()
	scriptDir := filepath.Join(root, "commands", "compact")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "leaked.pid")
	// The script exits at once and leaves a background sleep that inherited
	// its stdout and stderr, which is what keeps the pipe open.
	script := "#!/bin/sh\nsleep 60 &\necho $! > '" + pidFile + "'\n"
	if err := os.WriteFile(filepath.Join(scriptDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake run.sh: %v", err)
	}
	t.Cleanup(func() { killLeakedChild(t, pidFile) })

	prev := doltCallWaitDelay
	doltCallWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { doltCallWaitDelay = prev })

	fakeDolt := filepath.Join(root, "bin", "dolt")
	cityPath, dataDir := t.TempDir(), t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := runCompactScriptForRealDoltTest(t, fakeDolt, root, cityPath, dataDir, 0, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("runCompactScriptForRealDoltTest err = %v, want exec.ErrWaitDelay: the script exited but a child still held its output pipe", err)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatalf("runCompactScriptForRealDoltTest still blocked %v after the script exited: a child holding the output pipe keeps Wait from returning when no WaitDelay is set", testutil.ExecRaceTimeout)
	}
}

// killLeakedChild kills the background sleep the fake run.sh left behind, by the
// PID the script itself recorded.
func killLeakedChild(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Logf("leaked child pid file: %v", err)
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Logf("leaked child pid %q: %v", raw, err)
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
