package beads

import (
	"strings"
	"testing"
)

// These tests exercise liveDoltGuard in isolation: they never fork bd or
// connect to a Dolt server, so they are safe to run from an agent shell that
// has GC_DOLT_PORT pointing at the live city Dolt.

func TestLiveDoltGuard_RefusesInheritedLeakedPort(t *testing.T) {
	restore := setInheritedDoltPortsForTesting("28231", "28231")
	defer restore()

	// bare `go test` from an agent shell: GC_DOLT_PORT was inherited at startup
	// and flows through to bd unchanged.
	env := []string{"PATH=/usr/bin", "GC_DOLT_PORT=28231"}
	if err := liveDoltGuard("bd", env); err == nil {
		t.Fatal("expected guard to refuse bd against inherited live port 28231, got nil")
	}
}

func TestLiveDoltGuard_RefusesInheritedBeadsServerPort(t *testing.T) {
	restore := setInheritedDoltPortsForTesting("", "28231")
	defer restore()

	env := []string{"BEADS_DOLT_SERVER_PORT=28231"}
	if err := liveDoltGuard("bd", env); err == nil {
		t.Fatal("expected guard to refuse bd against inherited BEADS_DOLT_SERVER_PORT 28231, got nil")
	}
}

func TestLiveDoltGuard_AllowsEphemeralUnsetPort(t *testing.T) {
	restore := setInheritedDoltPortsForTesting("28231", "28231")
	defer restore()

	// The sanctioned `make`/script harness drops the port via env -i, so bd
	// derives an ephemeral per-city-path port. Nothing in the resolved env.
	env := []string{"PATH=/usr/bin"}
	if err := liveDoltGuard("bd", env); err != nil {
		t.Fatalf("expected guard to allow ephemeral (unset) port, got %v", err)
	}
}

func TestLiveDoltGuard_AllowsTestOwnedPort(t *testing.T) {
	restore := setInheritedDoltPortsForTesting("28231", "28231")
	defer restore()

	// A test that started its own Dolt and overrode the port to a server it
	// owns must not be blocked: the effective port differs from the leaked one.
	env := []string{"GC_DOLT_PORT=31364"}
	if err := liveDoltGuard("bd", env); err != nil {
		t.Fatalf("expected guard to allow test-owned port 31364, got %v", err)
	}
}

func TestLiveDoltGuard_InertWhenNothingInheritedAtStartup(t *testing.T) {
	restore := setInheritedDoltPortsForTesting("", "")
	defer restore()

	// CI and clean shells inherit no Dolt port at startup, so even a port set
	// later (e.g. a test's t.Setenv) is the test's own and must pass.
	env := []string{"GC_DOLT_PORT=28231"}
	if err := liveDoltGuard("bd", env); err != nil {
		t.Fatalf("expected guard inert when no port inherited at startup, got %v", err)
	}
}

func TestLiveDoltGuard_IgnoresNonDoltCommands(t *testing.T) {
	restore := setInheritedDoltPortsForTesting("28231", "28231")
	defer restore()

	env := []string{"GC_DOLT_PORT=28231"}
	if err := liveDoltGuard("sh", env); err != nil {
		t.Fatalf("expected guard to ignore non-bd/dolt command, got %v", err)
	}
}

func TestLiveDoltGuard_EscapeHatchAllows(t *testing.T) {
	restore := setInheritedDoltPortsForTesting("28231", "28231")
	defer restore()
	t.Setenv(allowInheritedDoltEnvVar, "1")

	env := []string{"GC_DOLT_PORT=28231"}
	if err := liveDoltGuard("bd", env); err != nil {
		t.Fatalf("expected escape hatch %s=1 to allow, got %v", allowInheritedDoltEnvVar, err)
	}
}

// TestExecCommandRunner_LiveDoltGuardRefusesBeforeExec proves the guard is
// wired into the real runner: it refuses BEFORE any exec, so no bd binary is
// needed and the live Dolt server is never contacted. The override forces the
// leaked port into the resolved env to mimic a bare `go test` from an agent
// shell.
func TestExecCommandRunner_LiveDoltGuardRefusesBeforeExec(t *testing.T) {
	restore := setInheritedDoltPortsForTesting("28231", "28231")
	defer restore()

	runner := ExecCommandRunnerWithEnv(map[string]string{"GC_DOLT_PORT": "28231"})
	_, err := runner(t.TempDir(), "bd", "list")
	if err == nil {
		t.Fatal("expected ExecCommandRunner to refuse bd against leaked Dolt port, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to exec") {
		t.Fatalf("expected live-Dolt guard error, got: %v", err)
	}
}

func TestEnvSliceValue(t *testing.T) {
	env := []string{"A=1", "GC_DOLT_PORT=28231", "GC_DOLT_PORT=4406", "B=2"}
	// Last matching entry wins, mirroring os/exec env semantics.
	if got := envSliceValue(env, "GC_DOLT_PORT"); got != "4406" {
		t.Fatalf("envSliceValue = %q, want 4406 (last wins)", got)
	}
	if got := envSliceValue(env, "MISSING"); got != "" {
		t.Fatalf("envSliceValue(MISSING) = %q, want empty", got)
	}
}
