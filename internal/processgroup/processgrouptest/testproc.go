// Package processgrouptest provides test helpers for subprocess cleanup tests.
package processgrouptest

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// RequireRealProcessSignals skips tests that intentionally send OS signals
// unless the process-backed test lane explicitly opted in.
func RequireRealProcessSignals(t testing.TB) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("GC_REAL_PROCESS_SIGNAL_TESTS")) == "1" {
		return
	}
	if strings.TrimSpace(os.Getenv("GC_FAST_UNIT")) == "0" {
		return
	}
	t.Skip("skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1")
}

// KillFromPIDFile terminates the process whose PID is recorded at path.
func KillFromPIDFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read child pid file %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse child pid file %s: %v", path, err)
	}
	if pid <= 1 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find child process %d from %s: %v", pid, path, err)
	}
	_ = process.Kill()
}

// WaitForFileSize waits until path exists with non-empty contents.
func WaitForFileSize(t *testing.T, path string) int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		info, err := os.Stat(path)
		if err == nil {
			if info.Size() > 0 {
				return info.Size()
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat heartbeat file %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for heartbeat file %s to grow", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// AssertFileSizeStable fails if path keeps growing during stableFor.
func AssertFileSizeStable(t *testing.T, path string, initialSize int64, stableFor time.Duration) {
	t.Helper()
	lastSize := initialSize
	stableSince := time.Now()
	deadline := time.Now().Add(3 * time.Second)
	for {
		time.Sleep(50 * time.Millisecond)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat heartbeat file %s: %v", path, err)
		}
		if size := info.Size(); size != lastSize {
			lastSize = size
			stableSince = time.Now()
		}
		if time.Since(stableSince) >= stableFor {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat file %s kept growing after timeout cleanup; latest size %d", path, lastSize)
		}
	}
}
