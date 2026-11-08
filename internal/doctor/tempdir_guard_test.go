package doctor

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// guardedTempDir and testOwnedHome delegate to the shared gascity test
// helper (ga-zq8iwb) that deterministically retries a TempDir removal so it
// never races a lingering real-bd/eventkit writer. They keep their original
// private names so this package's existing call sites
// (checks_custom_types_test.go, checks_custom_types_bd_pin_test.go) need no
// changes; the retry/removal logic itself, and its own tests — including the
// injectable-remove seam (beadstest.GuardedTempDirWith) this package no
// longer needs a private wrapper for — now live once in internal/beads/beadstest
// instead of being duplicated here.
func guardedTempDir(t *testing.T) string {
	t.Helper()
	return beadstest.GuardedTempDir(t)
}

func testOwnedHome(t *testing.T) string {
	t.Helper()
	return beadstest.TestOwnedHome(t)
}

// guardedWorkspaceDir returns a dir for the real bd these tests run in,
// isolated from every directory above it, and sitting below a bait ancestor
// that fails the test in every run if that isolation stops holding. See
// beadstest.GuardedBdWorkspaceDir and ga-l7otw9. Only dirs a real bd runs in
// need it; the fake-bd tests in this package do not.
func guardedWorkspaceDir(t *testing.T) string {
	t.Helper()
	return beadstest.GuardedBdWorkspaceDir(t)
}
