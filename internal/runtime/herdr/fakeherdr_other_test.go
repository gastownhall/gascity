//go:build !windows

package herdr

import (
	"path/filepath"
	"syscall"
	"testing"
)

// The Go fake herdr is for Windows. Elsewhere the tests run their shell
// scripts, and these are never reached.

func newGoFakeHerdr(t *testing.T, _ fakeHerdrSpec) string {
	t.Helper()
	t.Fatal("the Go fake herdr runs only on Windows")
	return ""
}

func goFakeHerdrFromScript(t *testing.T, _ string) string {
	t.Helper()
	t.Fatal("the Go fake herdr runs only on Windows")
	return ""
}

func rewriteGoFake(t *testing.T, _ *Provider, _, _ string) {
	t.Helper()
	t.Fatal("the Go fake herdr runs only on Windows")
}

// makeWaitHold creates the FIFO the shell fake's `agent wait` on a blocked
// agent holds on: nothing writes it, so the read blocks until the caller's ctx
// kills the fake, with no child process left to outlive the kill.
func makeWaitHold(t *testing.T, state string) {
	t.Helper()
	if err := syscall.Mkfifo(filepath.Join(state, "wait_hold"), 0o600); err != nil {
		t.Fatal(err)
	}
}
