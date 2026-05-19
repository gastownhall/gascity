//go:build !windows

package beads

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecCommandRunnerTimeoutKillsChildProcess(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	oldTimeout := bdCommandTimeout
	bdCommandTimeout = 50 * time.Millisecond
	t.Cleanup(func() { bdCommandTimeout = oldTimeout })

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "spawn-child.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
sleep 30 &
echo "$!" > "$1"
wait
`), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	runner := ExecCommandRunner()
	_, err := runner(dir, script, pidFile)
	if err == nil {
		t.Fatal("runner unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("error = %v, want timeout", err)
	}

	pidBytes, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("read child pid: %v", readErr)
	}
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatal("child pid was empty")
	}

	for range 20 {
		if err := exec.Command("kill", "-0", pid).Run(); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	_ = exec.Command("kill", "-KILL", pid).Run()
	t.Fatalf("child process %s survived command timeout", pid)
}

func TestKillCommandTreeHandlesNilCommand(t *testing.T) {
	if err := killCommandTree(nil); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killCommandTree(nil): %v", err)
	}
}
