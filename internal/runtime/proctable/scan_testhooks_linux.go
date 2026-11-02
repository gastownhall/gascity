//go:build linux

package proctable

import "testing"

// SetSubreaperPIDForTesting binds a snapshot's known adopter to its orphan scan.
// Test harnesses such as Bazel use their own subreaper, outside the production
// systemd-user topology. The caller must establish that this is its test-owned
// process ancestry; production detection and reaping rules are unchanged.
func SetSubreaperPIDForTesting(t testing.TB, pid int) {
	t.Helper()
	if pid <= 1 {
		t.Fatal("test subreaper must be an identified process ancestor")
	}
	previous := detectSubreaper
	detectSubreaper = func(int) int { return pid }
	t.Cleanup(func() { detectSubreaper = previous })
}
