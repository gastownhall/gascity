//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// occupiedLoopbackPort binds a fresh 127.0.0.1 port WITHOUT SO_REUSEADDR for the
// rest of the test, so a python child that binds the number with SO_REUSEADDR
// fails with "Address already in use". It does not go through the fixtures'
// own port helpers, which hold their number with SO_REUSEADDR.
func occupiedLoopbackPort(t *testing.T) int {
	t.Helper()
	fd, port, err := rawBind(syscall.AF_INET, 0, false)
	if err != nil {
		t.Fatalf("occupy a 127.0.0.1 port: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	return port
}

// TestListenerFixturesReportChildStderrAndExitStatus pins AC4/AC6: when a python
// listener fixture's child dies before it is ready, the failure carries the
// child's exit status and stderr, and ends the wait when the child exits
// instead of running out the five-second readiness deadline.
func TestListenerFixturesReportChildStderrAndExitStatus(t *testing.T) {
	cases := []struct {
		name       string
		wantStderr string
		start      func(t *testing.T, scratch string) (*exec.Cmd, error)
	}{
		{"startTCPListenerProcess", "Address already in use", func(t *testing.T, scratch string) (*exec.Cmd, error) {
			return launchTCPListenerProcess(scratch, occupiedLoopbackPort(t))
		}},
		{"startTCPListenerProcessInDir", "Address already in use", func(t *testing.T, scratch string) (*exec.Cmd, error) {
			return launchTCPListenerProcessInDir(scratch, occupiedLoopbackPort(t), filepath.Join(scratch, "data"))
		}},
		{"startLockedDelayedTCPListenerProcessInDir", "IsADirectoryError", func(t *testing.T, scratch string) (*exec.Cmd, error) {
			// A directory cannot be opened for append, so the child dies on its lock file.
			lockDir := filepath.Join(scratch, "lock")
			if err := os.MkdirAll(lockDir, 0o755); err != nil {
				t.Fatal(err)
			}
			return launchLockedDelayedTCPListenerProcessInDir(scratch, lockDir, occupiedLoopbackPort(t), filepath.Join(scratch, "data"), 0)
		}},
		{"startUnixSocketProcess", "FileNotFoundError", func(_ *testing.T, scratch string) (*exec.Cmd, error) {
			// A short path under a directory that does not exist: a t.TempDir path
			// can exceed the AF_UNIX length limit and fail with a different error.
			return launchUnixSocketProcess(scratch, "/gc-listener-test-missing-dir/sock")
		}},
		{"startOpenFileProcess", "IsADirectoryError", func(t *testing.T, scratch string) (*exec.Cmd, error) {
			dir := filepath.Join(scratch, "held-dir")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return launchOpenFileProcess(scratch, dir)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			begin := time.Now()
			proc, err := tc.start(t, t.TempDir())
			if err == nil {
				_ = proc.Process.Kill()
				_ = proc.Wait()
				t.Fatal("launch succeeded, want the child's failure")
			}
			for _, want := range []string{"did not become ready", "child exit: exit status 1", "child stderr:", tc.wantStderr} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q:\n%v", want, err)
				}
			}
			if elapsed := time.Since(begin); elapsed > 4*time.Second {
				t.Errorf("failure took %v, want it reported when the child exits, not at the readiness deadline", elapsed)
			}
		})
	}
}
