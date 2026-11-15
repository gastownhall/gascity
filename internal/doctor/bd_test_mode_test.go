package doctor

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestPinEmbeddedBDHomeRunsBDSubprocessesInTestMode pins the half of the
// real-bd fixture teardown contract that retrying the TempDir removal cannot
// provide. Without bd's test mode every bd invocation launches a detached
// `bd send-metrics` child that outlives the bd that spawned it, and under load
// the test process too, and keeps writing $HOME/.beads/eventsData while
// t.TempDir's single-shot RemoveAll runs. The cleanup then fails with
// "directory not empty" (ga-1f81md fixed this for cmd/gc; ga-u5azpz is the same
// mechanism in this package). Test mode never launches that child, so there is
// no writer left to wait for.
//
// The helper is for embedded workspaces only. For a server-bound workspace bd
// dials the 127.0.0.1:1 sentinel in test mode and breaks, so
// TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint must not use it.
func TestPinEmbeddedBDHomeRunsBDSubprocessesInTestMode(t *testing.T) {
	// Start from a state that is not test mode whatever the ambient
	// environment holds, so the assertion below passes only if the helper sets
	// it. t.Setenv restores the original.
	t.Setenv(beadstest.EnvBeadsTestMode, "")
	pinEmbeddedBDHome(t)

	// The fixtures start bd with exec.Command and no explicit Env, so a child
	// sees exactly this process's environment. Asserting on it in-process avoids
	// a probe subprocess, which the source-level resource census would count.
	if got := os.Getenv(beadstest.EnvBeadsTestMode); got != "1" {
		t.Errorf("a bd subprocess would see %s=%q, want %q", beadstest.EnvBeadsTestMode, got, "1")
	}
}

// TestPinEmbeddedBDHomeOwnsHome checks the helper still gives the fixture a
// test-owned HOME, the property testOwnedHome provided before: bd's config
// precedence falls through to $HOME/.beads/config.yaml, so a fleet HOME with
// dolt.shared-server set there would otherwise route an embedded fixture to a
// shared server (ga-zxpfic).
func TestPinEmbeddedBDHomeOwnsHome(t *testing.T) {
	home := pinEmbeddedBDHome(t)
	if home == "" {
		t.Fatal("pinEmbeddedBDHome returned an empty HOME")
	}
	if got := os.Getenv("HOME"); got != home {
		t.Errorf("a bd subprocess would see HOME=%q, want the test-owned %q", got, home)
	}
}
