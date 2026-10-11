package tmuxtest

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperHoldSentinel is not a real test: it is re-executed as a
// subprocess by TestSweepOrphanReapsRealSIGKILLedProcess to hold a genuine
// OS-level flock the same way a live gc test process does. Driving the
// SIGKILL scenario in ga-ntbpyb.1 acceptance criterion 3 against a real
// process (rather than a synthetic dead-PID fixture, as the other tests in
// this package use) proves the kernel actually releases the flock on kill
// and that the sweep's liveness check observes that release -- the two
// things a purely synthetic fixture cannot exercise. Mirrors the
// self-re-exec helper-process idiom used by
// internal/workspacesvc/orphan_reap_test.go and the Go standard library's
// own os/exec tests.
func TestHelperHoldSentinel(t *testing.T) {
	if os.Getenv("GASCITY_TMUXTEST_HELPER_HOLD_SENTINEL") != "1" {
		t.Skip("not invoked as a subprocess helper")
	}
	root := os.Getenv("GASCITY_TMUXTEST_HELPER_ROOT")
	prefix := os.Getenv("GASCITY_TMUXTEST_HELPER_PREFIX")
	dir := filepath.Join(root, prefix+strconv.Itoa(os.Getpid())+"-orphan")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "helper: mkdir:", err)
		os.Exit(2)
	}
	sentinel, err := HoldAliveSentinel(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: HoldAliveSentinel:", err)
		os.Exit(2)
	}
	defer func() { _ = sentinel.Close() }()
	// Held until the parent test SIGKILLs this process; the sleep duration
	// only bounds how long a leaked/un-killed helper would survive.
	time.Sleep(2 * time.Minute)
}

// TestSweepOrphanReapsRealSIGKILLedProcess covers ga-ntbpyb.1 acceptance
// criterion 3's SIGKILL/timeout scenario end to end: a real child process
// creates a gct-<pid>-* socket parent dir, takes the alive-sentinel flock
// exactly as cmd/gc, internal/runtime/tmux, and test/integration do via
// NewSocketParentDir, and is then SIGKILLed out from under the lock -- the
// uncatchable-signal case a trap/defer cannot protect against. It asserts
// both that the specific orphaned dir is reaped and that the leak count
// across the root is tightly bounded (zero orphans, then exactly the one
// dir a subsequent NewSocketParentDir legitimately creates for itself).
func TestSweepOrphanReapsRealSIGKILLedProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	root := t.TempDir()
	const prefix = "gct-sigkill-"

	cmd := exec.Command(exe, "-test.run=^TestHelperHoldSentinel$")
	cmd.Env = append(os.Environ(),
		"GASCITY_TMUXTEST_HELPER_HOLD_SENTINEL=1",
		"GASCITY_TMUXTEST_HELPER_ROOT="+root,
		"GASCITY_TMUXTEST_HELPER_PREFIX="+prefix,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	childPID := cmd.Process.Pid
	dir := filepath.Join(root, prefix+strconv.Itoa(childPID)+"-orphan")

	deadline := time.Now().Add(5 * time.Second)
	for {
		if exists, held := aliveSentinelHeld(dir); exists && held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not hold sentinel at %s in time; stderr=%s", dir, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL helper: %v", err)
	}
	_ = cmd.Wait() // reap; a kill-induced exit error is expected and irrelevant here.

	// SIGKILL delivery and lock release are not necessarily observable
	// synchronously from the parent's perspective; poll rather than assume.
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, held := aliveSentinelHeld(dir); !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sentinel at %s still held 5s after SIGKILL", dir)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The min-age race guard intentionally preserves dirs this fresh (to
	// avoid reaping a concurrent sibling's just-created dir). Backdate to
	// simulate the SIGKILL/timeout having happened over an hour ago -- the
	// real-world orphan scenario the sweep exists to clean up.
	backdatePastSweepAge(t, dir)

	if got := countPrefixedDirs(t, root, prefix); got != 1 {
		t.Fatalf("orphaned dirs before sweep = %d, want 1", got)
	}

	SweepOrphanPIDPrefixedDirs(root, prefix, io.Discard)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("SIGKILL'd process's socket parent dir survived sweep: %s", dir)
	}
	if got := countPrefixedDirs(t, root, prefix); got != 0 {
		t.Fatalf("orphaned dirs after sweep = %d, want 0", got)
	}

	// A subsequent legitimate creation must succeed and leave the leak
	// count tightly bounded at exactly the one dir it owns.
	newDir, sentinel, err := NewSocketParentDir(root, io.Discard)
	if err != nil {
		t.Fatalf("NewSocketParentDir after reap: %v", err)
	}
	defer func() { _ = sentinel.Close() }()
	t.Cleanup(func() { _ = os.RemoveAll(newDir) })

	if !strings.HasPrefix(filepath.Base(newDir), SocketParentDirPrefix) {
		t.Fatalf("NewSocketParentDir returned dir %q not under prefix %q", newDir, SocketParentDirPrefix)
	}
	if got := countPrefixedDirs(t, root, SocketParentDirPrefix); got != 1 {
		t.Fatalf("orphaned+live dirs after re-creation = %d, want exactly 1", got)
	}
}

func countPrefixedDirs(t *testing.T, root, prefix string) int {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", root, err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			n++
		}
	}
	return n
}
