//go:build !windows

package beads_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// TestBdStoreListContextKillsChildOnTimeout is the architecture-mandated
// verifying test (ga-f1frwf/ga-oeeggk): stub bd with an over-budget sleep and
// assert (a) ListContext returns a degraded result within budget and (b) the
// spawned bd child is actually gone afterward — proven via the new
// ContextLister interface (BdStore.ListContext + a real ctx-aware exec
// runner), not the sibling mitigation bead's throwaway per-request store.
func TestBdStoreListContextKillsChildOnTimeout(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	if _, err := exec.LookPath("kill"); err != nil {
		t.Skip("kill unavailable")
	}

	pidFile := filepath.Join(t.TempDir(), "bd.pid")
	binDir := t.TempDir()
	stubPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(stubPath, []byte("#!/bin/sh\necho \"$$\" > \"$BD_TEST_PIDFILE\"\nsleep 30\nprintf '[]\\n'\n"), 0o755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runnerCtx := beads.ExecCommandRunnerContext(map[string]string{"BD_TEST_PIDFILE": pidFile})
	fallbackRunner := func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("plain CommandRunner should not be used when ListContext is called with a configured runner context")
		return nil, nil
	}
	s := beads.NewBdStore(t.TempDir(), fallbackRunner, beads.WithBdStoreRunnerContext(runnerCtx))

	const budget = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	_, err := s.ListContext(ctx, beads.ListQuery{AllowScan: true})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ListContext unexpectedly succeeded against a 30s-sleeping bd stub")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("ListContext blocked %s; want a degraded result bounded near the %s budget", elapsed, budget)
	}

	processgrouptest.WaitForFileSize(t, pidFile)
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatal("bd stub never wrote its PID")
	}
	for i := 0; i < 100; i++ {
		if err := exec.Command("kill", "-0", pid).Run(); err != nil {
			return // child is gone — success: ListContext's ctx cancellation killed it.
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = exec.Command("kill", "-KILL", pid).Run()
	t.Fatalf("bd child process %s survived ListContext's ctx cancellation (no lingering child expected)", pid)
}
