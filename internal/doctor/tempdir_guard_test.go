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
// isolated from every directory above it: bd adopts an ancestor workspace that
// has project files but no database, so a stray one initialized as the
// ancestor of a custom-types test's own temp dir and then failed every later
// run (ga-l7otw9). It is guardedTempDir plus that isolation, and a bait
// ancestor that fails the test in every run if the isolation ever stops
// holding. Only dirs a real bd runs in need it; the fake-bd tests do not.
func guardedWorkspaceDir(t *testing.T) string {
	t.Helper()
	return beadstest.GuardedBdWorkspaceDir(t)
}
