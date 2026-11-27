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
