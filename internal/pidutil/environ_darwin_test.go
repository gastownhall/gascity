//go:build darwin

package pidutil

import (
	"io"
	"os"
	"os/exec"
	"slices"
	"testing"
)

// environHelperEnv marks the child TestEnvironKeepsEachEntryWhole starts, in
// which TestEnvironHelperProcess holds until its stdin closes.
const environHelperEnv = "GC_PIDUTIL_ENVIRON_HELPER"

// Environ reads a live process's environment through kern.procargs2: a value
// holding spaces comes back whole, and an argument shaped like an entry is
// not read as one. The child is this test binary rather than an Apple
// platform binary, whose environment the kernel withholds from other
// processes.
func TestEnvironKeepsEachEntryWhole(t *testing.T) {
	want := []string{environHelperEnv + "=1", "TMUX_TMPDIR=/tmp/gc sockets/a b", "GC_PIDUTIL_LAST=x"}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEnvironHelperProcess$", "TMUX_TMPDIR=/from/argv")
	cmd.Env = want
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the helper: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	env, err := Environ(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Environ(helper): %v", err)
	}
	// The kernel's own key=value strings can follow the environment.
	if len(env) < len(want) || !slices.Equal(env[:len(want)], want) {
		t.Fatalf("Environ(helper) = %q, want it to begin with %q", env, want)
	}
}

// TestEnvironHelperProcess is the child TestEnvironKeepsEachEntryWhole reads:
// it holds until its stdin closes. Run directly, it does nothing.
func TestEnvironHelperProcess(_ *testing.T) {
	if os.Getenv(environHelperEnv) != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
