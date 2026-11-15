//go:build unix

package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgepoller"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// These tests discriminate one poller from another by reading a live process's
// argv. That capability is Unix-only: pidutil.Cmdline reads /proc or ps, and
// Windows reports ErrUnsupported rather than reading the PEB across a process
// boundary. The Windows half of this contract -- that an unidentifiable live
// PID blocks the gate instead of reading as "no poller" -- is pinned in
// submit_poller_identity_windows_test.go.

func startSubmitPollerLikeProcess(t *testing.T, cityPath, sessionName, agentName string) *exec.Cmd {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "gc-fake")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nread _hold\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(fake poller): %v", err)
	}
	cmd := exec.Command(scriptPath, nudgepoller.CommandArgs(cityPath, sessionName, agentName)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe(fake poller): %v", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatalf("Start(fake poller): %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	waitForSubmitPollerCmdline(t, cmd.Process.Pid, cityPath, sessionName, agentName)
	return cmd
}

func waitForSubmitPollerCmdline(t *testing.T, pid int, cityPath, sessionName, agentName string) {
	t.Helper()
	matches := nudgepoller.CmdlineMatcher(cityPath, sessionName, agentName)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if matched, err := pidutil.AliveWithCmdline(pid, matches); err == nil && matched {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("poller PID %d did not expose matching command line", pid)
}

func TestExistingSessionSubmitPollerPIDRejectsUnrelatedLivePID(t *testing.T) {
	cityPath := t.TempDir()
	pidPath := sessionSubmitPollerPIDPath(cityPath, "s-test", "session-id")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, "s-test", "session-id")
	if err != nil {
		t.Fatalf("existingSessionSubmitPollerPID: %v", err)
	}
	if running {
		t.Fatalf("existingSessionSubmitPollerPID(%q) = true for unrelated live PID %d", pidPath, os.Getpid())
	}
}

func TestExistingSessionSubmitPollerPIDAcceptsMatchingCitySession(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city with spaces")
	sessionName := "s-test"
	pidPath := sessionSubmitPollerPIDPath(cityPath, sessionName, "session-id")
	cmd := startSubmitPollerLikeProcess(t, cityPath, sessionName, "session-id")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, sessionName, "session-id")
	if err != nil {
		t.Fatalf("existingSessionSubmitPollerPID: %v", err)
	}
	if !running {
		t.Fatalf("existingSessionSubmitPollerPID(%q) = false for matching poller PID %d", pidPath, cmd.Process.Pid)
	}
}

func TestExistingSessionSubmitPollerPIDRejectsDifferentCitySameSession(t *testing.T) {
	cityPath := t.TempDir()
	otherCityPath := t.TempDir()
	sessionName := "s-test"
	pidPath := sessionSubmitPollerPIDPath(cityPath, sessionName, "session-id")
	cmd := startSubmitPollerLikeProcess(t, otherCityPath, sessionName, "session-id")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, sessionName, "session-id")
	if err != nil {
		t.Fatalf("existingSessionSubmitPollerPID: %v", err)
	}
	if running {
		t.Fatalf("existingSessionSubmitPollerPID(%q) = true for same-session poller in different city", pidPath)
	}
}

func TestExistingSessionSubmitPollerPIDRejectsDifferentTargetSameCitySession(t *testing.T) {
	cityPath := t.TempDir()
	sessionName := "s-test"
	pidPath := sessionSubmitPollerPIDPath(cityPath, sessionName, "session-id")
	cmd := startSubmitPollerLikeProcess(t, cityPath, sessionName, "old-alias")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, sessionName, "session-id")
	if err != nil {
		t.Fatalf("existingSessionSubmitPollerPID: %v", err)
	}
	if running {
		t.Fatalf("existingSessionSubmitPollerPID(%q) = true for same-session poller with different target key", pidPath)
	}
}
