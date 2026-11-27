package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHookDiscoveryAnswersBeforeDeadline pins #5533's deadline contract: a
// `gc hook` invocation that declares its budget — through --timeout, or the
// GC_HOOK_DEADLINE `gc hook run` hands its child — must ANSWER inside it:
// exit 2 with GC_HOOK_STORE_UNAVAILABLE and the hook deadline named on stderr,
// nothing on stdout. Before the fix no layer of the hook knew the caller's
// deadline, so a wedged store read was killed by the outer timeout (rc=124)
// with no protocol result at all, and a --claim could die mid-mutation.
//
// The store read hangs forever (sleep 30), so only the invocation deadline can
// end it. The read stops at the deadline minus the archive margin
// (hookInvocationReadContext), and the query's process group is killed with
// it, so no bd leg outlives the hook that stopped reading it.
func TestHookDiscoveryAnswersBeforeDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag bool
	}{
		{"flag --timeout", true},
		{"inherited GC_HOOK_DEADLINE", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The deadline arithmetic is anchored at hookProcessStart, which for
			// a test binary is when the binary started; anchor it to now so the
			// 2s budget and its margins are measured from this test.
			prevStart := hookProcessStart
			hookProcessStart = time.Now()
			t.Cleanup(func() { hookProcessStart = prevStart })

			pidPath := filepath.Join(t.TempDir(), "query.pid")
			hookExitCodeCity(t, fmt.Sprintf("echo $$ > %q; exec sleep 30", pidPath))
			deadline := hookProcessStart.Add(2 * time.Second)
			args := []string{"hook", "worker"}
			if tc.flag {
				args = append(args, "--timeout", "2s")
			} else {
				t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(deadline))
			}

			var stdout, stderr bytes.Buffer
			started := time.Now()
			code := run(args, &stdout, &stderr)

			if code != 2 {
				t.Fatalf("gc hook rendered exit %d, want 2 (the store gave no answer before the deadline; an outer timeout kill would render 124); stderr=%s", code, stderr.String())
			}
			if got := stdout.String(); got != "" {
				t.Fatalf("stdout = %q, want nothing: a read stopped at the deadline is not a protocol result", got)
			}
			if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
				t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
			}
			if !strings.Contains(stderr.String(), "stopped at the hook deadline") {
				t.Fatalf("stderr %q does not name the hook deadline that stopped the read", stderr.String())
			}
			if elapsed := time.Since(started); elapsed >= 2*time.Second {
				t.Fatalf("gc hook answered in %s, want it before the %s deadline it was given", elapsed, deadline.Sub(hookProcessStart))
			}
			raw, err := os.ReadFile(pidPath)
			if err != nil {
				t.Fatalf("reading %s: %v (the store read never started before its deadline)", pidPath, err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatalf("query pid file holds %q: %v", string(raw), err)
			}
			waitDeadlineQueryGone(t, pid)
		})
	}
}

// waitDeadlineQueryGone reports that the store-read process the deadline
// stopped is dead, not merely abandoned: its group was killed, so the pid the
// query recorded no longer answers signal 0. Reaping can lag the hook's return
// by a scheduler tick, so the check is bounded but does not pass on a live pid.
func waitDeadlineQueryGone(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("store-read process %d still alive after the hook answered; the deadline did not kill its group (kill: %v)", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHookNegativeTimeoutIsRefused pins the flag's own contract: a negative
// --timeout is a usage error, never a deadline in the past that would fail
// every read of the invocation before it starts.
func TestHookNegativeTimeoutIsRefused(t *testing.T) {
	hookExitCodeCity(t, "printf '[]'")
	var stdout, stderr bytes.Buffer
	code := run([]string{"hook", "worker", "--timeout", "-1s"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("gc hook rendered exit %d for --timeout -1s, want 1 (a usage error, not a run); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--timeout") {
		t.Fatalf("stderr %q does not name the refused --timeout flag", stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want nothing for a usage error", got)
	}
}

// deadlineClaimFixture is the worked example of #5533's claim-side design:
// `gc hook --claim --json --timeout 15s`. Reads end at W = start + 12s (the
// deadline less the 3s archive margin), so the work time is 12s and the claim
// machinery must derive a 4s mutation reserve (min(10s, work/3)) and an 8s
// claim fence (min(window, work − reserve)) from the Deadline alone. The
// recorder's ops carry that clock; readAdvance is how far the fake work query
// moves it before answering.
func deadlineClaimFixture(t *testing.T, readAdvance time.Duration) (*turnBoundClaimRecorder, hookClaimOps) {
	t.Helper()
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	clk := time.Now()
	ops.Now = func() time.Time { return clk }
	ops.Deadline = clk.Add(12 * time.Second)
	ops.Runner = func(string, string) (string, error) {
		clk = clk.Add(readAdvance)
		return turnBoundRoutedWork, nil
	}
	return rec, ops
}

// TestHookClaimRefusesCandidatePastDeadlineFence pins AC-4: with a declared
// invocation deadline, a claimable candidate the read reaches only AFTER the
// claim fence (8s in, on the 15s shape) is refused before any mutation — no
// claim write, nothing on stdout, a non-zero exit, and the
// execution.claim_window_expired fact recorded. The fence keeps the turn
// binding: a claim whose read outlived the fence cannot be handed to the turn
// that asked for it.
func TestHookClaimRefusesCandidatePastDeadlineFence(t *testing.T) {
	rec, ops := deadlineClaimFixture(t, 8*time.Second+500*time.Millisecond)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1 (the candidate arrived past the deadline's claim fence); stderr=%s", code, stderr.String())
	}
	if len(rec.claims) != 0 {
		t.Fatalf("claims = %v, want none: no claim mutation may be attempted past the fence", rec.claims)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing: a refused candidate is not a work or drain result", stdout.String())
	}
	if rec.drainAcked {
		t.Fatal("drain acknowledged for a candidate refused past the fence")
	}
	if len(rec.windowExpired) != 1 {
		t.Fatalf("claim_window_expired records = %d, want 1 (the refusal must be the recorded fact)", len(rec.windowExpired))
	}
	if age := rec.windowExpired[0].InvocationAge; age < 8*time.Second {
		t.Fatalf("event invocation age = %s, want the age past the 8s fence", age)
	}
	// The stderr names the DERIVED fence, so a mis-derived window (the
	// unbounded 160s default, or the 12s work time) cannot pass silently.
	if !strings.Contains(stderr.String(), "8s claim window") {
		t.Fatalf("stderr %q does not name the deadline-derived 8s claim window", stderr.String())
	}
}
