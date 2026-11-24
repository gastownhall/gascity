//go:build unix

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

// slowLegScript records its PID, then becomes a 30s sleep: a store read that
// runs far past any hook deadline.
const slowLegScript = `printf '%s\n' "$$" >> "$HOOK_PID_LOG"; exec sleep 30`

// hookDeadlineTestBudget is the caller's budget in
// TestHookDiscoveryAnswersBeforeDeadline: the hook must answer inside it.
const hookDeadlineTestBudget = 2 * time.Second

// deadlineHookCity writes a city from cityTOML whose agent "worker" runs its
// work_query (or the default one against a fake bd running bdScript), points
// the hook at it, and returns the file each slow leg writes its PID to.
func deadlineHookCity(t *testing.T, cityTOML, bdScript string) string {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCityAgents(t, cityTOML)
	t.Setenv("GC_CITY", cityDir)
	if bdScript != "" {
		fakeBin := t.TempDir()
		if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte("#!/bin/sh\n"+bdScript+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	pidLog := filepath.Join(t.TempDir(), "pids")
	t.Setenv("HOOK_PID_LOG", pidLog)
	t.Setenv(hookDeadlineEnv, "")
	prevStart := hookProcessStart
	hookProcessStart = time.Now()
	t.Cleanup(func() { hookProcessStart = prevStart })
	return pidLog
}

type hookRun struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
}

func runHookForDeadline(opts hookCommandOptions) hookRun {
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := cmdHookWithOptions([]string{"worker"}, opts, &stdout, &stderr)
	return hookRun{code: code, stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start)}
}

// hookTestProcessGone reports whether pid has exited: no such process, or a
// zombie waiting to be reaped.
func hookTestProcessGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(raw, ')')
	return i >= 0 && i+2 < len(raw) && raw[i+2] == 'Z'
}

// assertSlowLegsKilled fails unless every PID the slow legs logged has exited.
func assertSlowLegsKilled(t *testing.T, pidLog string) {
	t.Helper()
	raw, err := os.ReadFile(pidLog)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read pid log: %v", err)
	}
	for _, field := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("pid log entry %q: %v", field, err)
		}
		// SIGKILL to the leg's process group is delivered by the time the
		// hook answers; the exit itself is the kernel's to finish.
		gone := time.Now().Add(2 * time.Second)
		for !hookTestProcessGone(pid) {
			if time.Now().After(gone) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("slow leg pid %d still running after the hook answered", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// assertAnsweredAtDeadline is AC-3's answer: exit 2 before the budget, nothing
// on stdout, the store-unavailable token and the hook deadline on stderr, and
// the slow leg's process group killed.
func assertAnsweredAtDeadline(t *testing.T, got hookRun, pidLog string) {
	const budget = hookDeadlineTestBudget
	t.Helper()
	if got.code != hookExitWorkQueryFailed {
		t.Errorf("exit = %d, want %d; stderr=%s", got.code, hookExitWorkQueryFailed, got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want nothing", got.stdout)
	}
	if !strings.Contains(got.stderr, hookStoreUnavailableToken) || !strings.Contains(got.stderr, "hook deadline") {
		t.Errorf("stderr = %q, want %s and the hook deadline named", got.stderr, hookStoreUnavailableToken)
	}
	if got.elapsed >= budget {
		t.Errorf("hook answered after %s, want before the %s deadline", got.elapsed.Round(time.Millisecond), budget)
	}
	assertSlowLegsKilled(t, pidLog)
}

// TestHookDiscoveryAnswersBeforeDeadline pins #5533's invocation deadline on
// the discovery door: given a budget (gc hook --timeout, or the
// GC_HOOK_DEADLINE gc hook run hands its child), a store read that would
// outlast it is killed with its process group, and the hook answers with its
// defined failure — exit 2, GC_HOOK_STORE_UNAVAILABLE — before the deadline,
// instead of being killed itself with no answer.
func TestHookDiscoveryAnswersBeforeDeadline(t *testing.T) {
	const (
		budget      = hookDeadlineTestBudget
		singleStore = "[[agent]]\nname = \"worker\"\nscope = \"city\"\n"
	)
	customQuery := func(query string) string {
		return "[[agent]]\nname = \"worker\"\nscope = \"city\"\nwork_query = " + strconv.Quote(query) + "\n"
	}

	t.Run("inherited GC_HOOK_DEADLINE", func(t *testing.T) {
		pidLog := deadlineHookCity(t, singleStore, slowLegScript)
		t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(budget)))
		assertAnsweredAtDeadline(t, runHookForDeadline(hookCommandOptions{}), pidLog)
	})
	t.Run("--timeout", func(t *testing.T) {
		pidLog := deadlineHookCity(t, singleStore, slowLegScript)
		assertAnsweredAtDeadline(t, runHookForDeadline(hookCommandOptions{Timeout: budget}), pidLog)
	})
	t.Run("--timeout 1ms", func(t *testing.T) {
		pidLog := deadlineHookCity(t, singleStore, slowLegScript)
		assertAnsweredAtDeadline(t, runHookForDeadline(hookCommandOptions{Timeout: time.Millisecond}), pidLog)
	})
	t.Run("a later --timeout is bounded by the inherited deadline", func(t *testing.T) {
		pidLog := deadlineHookCity(t, singleStore, slowLegScript)
		t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(budget)))
		assertAnsweredAtDeadline(t, runHookForDeadline(hookCommandOptions{Timeout: time.Minute}), pidLog)
	})
	t.Run("a custom work_query is bounded the same way", func(t *testing.T) {
		pidLog := deadlineHookCity(t, customQuery(slowLegScript), "")
		assertAnsweredAtDeadline(t, runHookForDeadline(hookCommandOptions{Timeout: budget}), pidLog)
	})
	for _, tc := range []struct{ name, value string }{
		{name: "malformed", value: "not-a-time"},
		{name: "already past", value: hookDeadlineEnvValue(time.Now().Add(-time.Minute))},
	} {
		t.Run("a "+tc.name+" GC_HOOK_DEADLINE is ignored with a note", func(t *testing.T) {
			deadlineHookCity(t, singleStore, "printf '[]'")
			t.Setenv(hookDeadlineEnv, tc.value)
			got := runHookForDeadline(hookCommandOptions{})
			if got.code != 1 || strings.TrimSpace(got.stdout) != "[]" {
				t.Errorf("exit = %d stdout = %q, want the store's empty-queue answer (exit 1, []); stderr=%s", got.code, got.stdout, got.stderr)
			}
			if !strings.Contains(got.stderr, "ignoring "+hookDeadlineEnv) {
				t.Errorf("stderr = %q, want a note that %s was ignored", got.stderr, hookDeadlineEnv)
			}
		})
	}
	t.Run("--inject ignores --timeout and reads nothing", func(t *testing.T) {
		pidLog := deadlineHookCity(t, singleStore, slowLegScript)
		got := runHookForDeadline(hookCommandOptions{Inject: true, Timeout: -time.Second})
		if got.code != 0 || got.stdout != "" || got.stderr != "" {
			t.Errorf("--inject = exit %d stdout %q stderr %q, want a silent exit 0", got.code, got.stdout, got.stderr)
		}
		if _, err := os.Stat(pidLog); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("--inject ran a store read (pid log: %v)", err)
		}
	})
	t.Run("a negative --timeout is a usage error", func(t *testing.T) {
		pidLog := deadlineHookCity(t, singleStore, slowLegScript)
		got := runHookForDeadline(hookCommandOptions{Timeout: -time.Second})
		if got.code != 1 || got.stdout != "" || !strings.Contains(got.stderr, "--timeout") {
			t.Errorf("negative --timeout = exit %d stdout %q stderr %q, want exit 1 naming --timeout", got.code, got.stdout, got.stderr)
		}
		if _, err := os.Stat(pidLog); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a usage error ran a store read (pid log: %v)", err)
		}
	})
	t.Run("another store's work is served when the slow leg is killed", func(t *testing.T) {
		query := `case "$GC_RIG_ROOT" in */rigs/riga) ` + slowLegScript + `;; esac; printf '%s' '` + routedCityRow + `'`
		pidLog := deadlineHookCity(t, "[[rigs]]\nname = \"riga\"\npath = \"rigs/riga\"\n\n"+customQuery(query), "")
		if err := os.MkdirAll(filepath.Join(os.Getenv("GC_CITY"), "rigs", "riga"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := runHookForDeadline(hookCommandOptions{Timeout: budget})
		if got.code != 0 || !strings.Contains(got.stdout, "hw-city") {
			t.Errorf("exit = %d stdout = %q, want the city store's work served; stderr=%s", got.code, got.stdout, got.stderr)
		}
		if !strings.Contains(got.stderr, "riga") {
			t.Errorf("stderr = %q, want the killed riga leg reported", got.stderr)
		}
		if got.elapsed >= budget {
			t.Errorf("hook answered after %s, want before the %s deadline", got.elapsed.Round(time.Millisecond), budget)
		}
		assertSlowLegsKilled(t, pidLog)
	})
}
