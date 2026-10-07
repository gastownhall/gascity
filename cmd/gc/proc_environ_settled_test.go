//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWaitForProcEnvironNeverReturnsATornRead pins the reader the scope
// watchdog identity test relies on. os.ReadFile of /proc/<pid>/environ takes
// several read(2)s for an environment past 512 bytes, and an execve landing
// between two of them tears the result: the first chunk comes from the old
// image and the next read hits EOF on its torn-down address space. The
// truncated environment is non-empty, so a reader that only waits out the
// empty in-execve window returns it — dropping PATH, appended last by the
// spawn path, and failing "PATH = \"\"" intermittently in CI.
func TestWaitForProcEnvironNeverReturnsATornRead(t *testing.T) {
	env := make([]string, 0, 201)
	for i := 0; i < 200; i++ {
		env = append(env, fmt.Sprintf("GC_TEST_ENVIRON_FILLER_%03d=%s", i, strings.Repeat("x", 1000)))
	}
	const marker = "GC_TEST_ENVIRON_LAST=present"
	env = append(env, marker)

	for i := 0; i < 300; i++ {
		// The child execs twice (sh, then sleep), like the fake dolt.
		cmd := exec.Command("/bin/sh", "-c", "exec sleep 30")
		cmd.Env = env
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child: %v", err)
		}
		got := waitForProcEnviron(t, cmd.Process.Pid)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if got["GC_TEST_ENVIRON_LAST"] != "present" {
			t.Fatalf("iteration %d: environ read for pid %d is torn: %d entries, last entry missing", i, cmd.Process.Pid, len(got))
		}
	}
}

// TestWaitForProcEnvironRetriesThroughExecWindows calls the reader back to
// back while its target runs 200 execve()s, so reads land inside the windows
// a single sh -> sleep exec only rarely exposes: on Linux 7.2+ the open fails
// with EACCES there, and on any kernel bash's environment import can split an
// entry. The environment stays under 512 bytes so each read is one read(2):
// an sh -> sh exec keeps /proc/<pid>/exe unchanged, so a multi-read tear
// across one would not be caught.
func TestWaitForProcEnvironRetriesThroughExecWindows(t *testing.T) {
	const script = `n=$1; if [ "$n" -gt 0 ]; then exec /bin/sh -c "$0" "$0" $((n-1)); fi; exec sleep 30`
	env := []string{"GC_TEST_ENVIRON_LAST=present"}
	for i := 0; i < 5; i++ {
		env = append(env, fmt.Sprintf("GC_TEST_ENVIRON_FILLER_%d=x", i))
	}
	cmd := exec.Command("/bin/sh", "-c", script, script, "200")
	cmd.Env = env
	cmd.Dir = "/"
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	cmdlinePath := filepath.Join("/proc", strconv.Itoa(pid), "cmdline")
	deadline := time.Now().Add(30 * time.Second)
	for reads := 1; ; reads++ {
		got := waitForProcEnviron(t, pid)
		if got["GC_TEST_ENVIRON_LAST"] != "present" {
			t.Fatalf("read %d of pid %d is torn: %d entries, GC_TEST_ENVIRON_LAST missing", reads, pid, len(got))
		}
		if cmdline, err := os.ReadFile(cmdlinePath); err == nil && strings.HasPrefix(string(cmdline), "sleep\x00") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d did not reach sleep within 30s (%d reads)", pid, reads)
		}
	}
}
