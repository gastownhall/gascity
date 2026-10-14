//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
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
		// Wait for the shell's first instruction before measuring the sh ->
		// sleep exec race. Start may return while /proc still exposes the
		// pre-exec child image and its inherited parent environment; that is
		// a complete snapshot of the wrong image, not a torn read.
		ready, notify, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", "printf x >&3; exec 3>&-; exec sleep 30")
		cmd.ExtraFiles = []*os.File{notify}
		cmd.Env = env
		if err := cmd.Start(); err != nil {
			_ = ready.Close()
			_ = notify.Close()
			t.Fatalf("start child: %v", err)
		}
		_ = notify.Close()
		var signal [1]byte
		_, err = ready.Read(signal[:])
		_ = ready.Close()
		if err != nil || signal[0] != 'x' {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("wait for shell startup: %v", err)
		}
		got := waitForProcEnviron(t, cmd.Process.Pid)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if got["GC_TEST_ENVIRON_LAST"] != "present" {
			t.Fatalf("iteration %d: environ read for pid %d is torn: %d entries, last entry missing", i, cmd.Process.Pid, len(got))
		}
	}
}
