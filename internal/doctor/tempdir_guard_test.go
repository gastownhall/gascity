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

// pinEmbeddedBDHome is testOwnedHome for the fixtures that run a real bd in an
// embedded workspace. It also puts bd in test mode so no bd the fixture spawns
// leaves a detached `bd send-metrics` child writing $HOME/.beads/eventsData
// after the test returns (ga-u5azpz; the cmd/gc counterpart is
// pinTestOwnedBDHome, ga-1f81md). Do NOT use it for a server-bound workspace:
// in test mode bd dials the 127.0.0.1:1 sentinel there and breaks.
func pinEmbeddedBDHome(t *testing.T) string {
	t.Helper()
	t.Setenv(beadstest.EnvBeadsTestMode, "1")
	return testOwnedHome(t)
}
