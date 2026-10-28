//go:build unix

package main

// These pin provider-op cleanup in terms of POSIX process groups and inode
// identity. The Windows equivalent of the cleanup contract -- a terminated
// tree leaves no descendant behind -- is pinned directly in
// internal/processgroup's Job Object tests.
import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunProviderOpKillsProcessGroupOnTimeout(t *testing.T) {
	cancelCh := useCancelableProviderLifecycleContext(t)

	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "provider-op.sh")
	content := `#!/bin/sh
sh -c 'echo $$ > "$GC_TEST_CHILD_PID"; while :; do sleep 1; done' &
wait
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	// Timeouts and explicit cancellation both drive exec.Cmd.Cancel; cancel only
	// after the child PID is observable so the cleanup assertion cannot race setup.
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- runProviderOpWithEnv(script, append(os.Environ(), "GC_TEST_CHILD_PID="+childPIDFile), "health")
	}()

	cancel := waitForProviderLifecycleCancel(t, cancelCh)
	t.Cleanup(cancel)
	pid := waitForProviderTestChildPID(t, childPIDFile)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	cancel()

	var err error
	select {
	case err = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("provider op did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected timeout error")
	}

	waitForProviderTestPIDExit(t, pid, "provider op")
}
func TestRunProviderOpWithEnvContextParentCancellationKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "provider-op.sh")
	content := `#!/bin/sh
sh -c 'echo $$ > "$GC_TEST_CHILD_PID"; while :; do sleep 1; done' &
wait
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- runProviderOpWithEnvContext(ctx, script, append(os.Environ(), "GC_TEST_CHILD_PID="+childPIDFile), "health")
	}()

	pid := waitForProviderTestChildPID(t, childPIDFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("provider op error = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider op did not return after parent cancellation")
	}
	waitForProviderTestPIDExit(t, pid, "provider op with parent context")
}
func TestRunProviderProbeKillsProcessGroupOnTimeout(t *testing.T) {
	cancelCh := useCancelableProviderLifecycleContext(t)

	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "provider-probe.sh")
	content := `#!/bin/sh
sh -c 'echo $$ > "$GC_TEST_CHILD_PID"; while :; do sleep 1; done' &
wait
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_CHILD_PID", childPIDFile)

	// Timeouts and explicit cancellation both drive exec.Cmd.Cancel; cancel only
	// after the child PID is observable so the cleanup assertion cannot race setup.
	resultCh := make(chan bool, 1)
	go func() {
		resultCh <- runProviderProbe(script, "", "")
	}()

	cancel := waitForProviderLifecycleCancel(t, cancelCh)
	t.Cleanup(cancel)
	pid := waitForProviderTestChildPID(t, childPIDFile)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	cancel()

	var ok bool
	select {
	case ok = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("provider probe did not return after cancellation")
	}
	if ok {
		t.Fatal("expected timeout probe to return false")
	}

	waitForProviderTestPIDExit(t, pid, "provider probe")
}
func waitForProviderTestPIDExit(t *testing.T, pid int, label string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("provider child pid %d survived %s cancellation", pid, label)
}
func TestGcBeadsBdStartDoesNotReplaceLiveLockFileInode(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed")
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation")
	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := `#!/bin/sh
set -eu
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    printf 'sql-server\n' >> "$GC_FAKE_DOLT_INVOCATION_FILE"
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.makedirs(data_dir, exist_ok=True)
    os.chdir(data_dir)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		t.Fatal(err)
	}

	readyFile := filepath.Join(t.TempDir(), "holder-ready")
	releaseFile := filepath.Join(t.TempDir(), "holder-release")
	holder := exec.Command("sh", "-c", `
set -eu
lock_file="$1"
ready_file="$2"
release_file="$3"
: > "$lock_file"
exec 9>"$lock_file"
flock 9
printf 'ready\n' > "$ready_file"
while [ ! -f "$release_file" ]; do
  sleep 0.1
 done
`, "sh", layout.LockFile, readyFile, releaseFile)
	holder.Env = sanitizedBaseEnv("PATH=" + os.Getenv("PATH"))
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- holder.Wait()
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release\n"), 0o644)
		select {
		case err := <-holderDone:
			if err != nil {
				t.Errorf("lock holder exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = holder.Process.Kill()
			<-holderDone
			t.Errorf("timed out waiting for lock holder to exit")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lock holder to acquire flock")
		}
		time.Sleep(25 * time.Millisecond)
	}

	inodeFor := func(path string) uint64 {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("Stat(%s) did not expose syscall.Stat_t", path)
		}
		return stat.Ino
	}
	beforeInode := inodeFor(layout.LockFile)

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT=3311",
		"GC_DOLT_CONCURRENT_START_READY_TIMEOUT_MS=1000",
		"GC_FAKE_DOLT_INVOCATION_FILE="+invocationFile,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("gc-beads-bd start unexpected error: %v", err)
		}
	}
	if exitCode == 0 {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
		t.Fatalf("gc-beads-bd start unexpectedly succeeded while another process held the start lock\n%s", out)
	}
	if exitCode != 1 {
		t.Fatalf("gc-beads-bd start exit %d, want 1\n%s", exitCode, out)
	}
	if !strings.Contains(string(out), "could not acquire dolt start lock") {
		t.Fatalf("gc-beads-bd start output = %q, want lock acquisition failure", out)
	}
	afterInode := inodeFor(layout.LockFile)
	if afterInode != beforeInode {
		t.Fatalf("lock inode changed from %d to %d while original holder was still live", beforeInode, afterInode)
	}
	if invocation, err := os.ReadFile(invocationFile); err == nil && strings.TrimSpace(string(invocation)) != "" {
		t.Fatalf("dolt should not have been invoked while the start lock was held:\n%s", string(invocation))
	}
}
