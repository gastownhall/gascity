package proctable

import "testing"

// A gc binary a test harness execs has no testing.Testing() and no .test
// argv[0], so liveScanGuard is blind in it. The harness stamps TestSpawnedEnv;
// the orphan sweep's empty-id scan must then refuse the live /proc.
func TestOrphanScanGuard_RefusesEmptyIDWhenTestSpawned(t *testing.T) {
	t.Setenv(TestSpawnedEnv, "1")
	if err := orphanScanGuard(""); err == nil {
		t.Fatal("orphanScanGuard(\"\") allowed the live orphan scan in a test-spawned binary")
	}
}

// The scan scoped to one session id only matches that session's own
// processes, so a test-spawned supervisor still needs it.
func TestOrphanScanGuard_AllowsSessionScopedScanWhenTestSpawned(t *testing.T) {
	t.Setenv(TestSpawnedEnv, "1")
	if err := orphanScanGuard("gm-abc"); err != nil {
		t.Fatalf("orphanScanGuard(session id) = %v, want nil", err)
	}
}

// A production gc (no marker) must not start refusing the sweep.
func TestOrphanScanGuard_AllowsProduction(t *testing.T) {
	t.Setenv(TestSpawnedEnv, "")
	if TestSpawned() {
		t.Fatal("TestSpawned() = true with the marker unset")
	}
	if err := orphanScanGuard(""); err != nil {
		t.Fatalf("orphanScanGuard(\"\") = %v with no marker, want nil", err)
	}
}

func TestTestSpawned_DetectsMarker(t *testing.T) {
	t.Setenv(TestSpawnedEnv, "1")
	if !TestSpawned() {
		t.Fatal("TestSpawned() = false with the marker set")
	}
}
