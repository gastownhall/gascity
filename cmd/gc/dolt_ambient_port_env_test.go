package main

import (
	"testing"
)

// setSupervisorAmbientDoltPortExportForTest pins the export gate for one test
// and restores what it found on cleanup.
//
// The gate is process-global and its production writer
// (enableSupervisorAmbientDoltPortExport, called from doSupervisorRun) has no
// paired reset — by design, since a real supervisor never un-becomes a
// supervisor. Any test that drives doSupervisorRun therefore leaves the gate
// latched on for the rest of the binary, so a test asserting either gate state
// must pin it explicitly rather than assume the zero value.
func setSupervisorAmbientDoltPortExportForTest(t *testing.T, enabled bool) {
	t.Helper()
	previous := supervisorAmbientDoltPortExportEnabled.Swap(enabled)
	t.Cleanup(func() { supervisorAmbientDoltPortExportEnabled.Store(previous) })
}

// fakeAmbientDoltEnv substitutes an in-memory environment for the ambient
// export seam and returns it, seeded with the given entries.
//
// Nothing here touches the real process environment. GC_DOLT_PORT and
// BEADS_DOLT_SERVER_PORT are leak vectors — a value left behind by a test can
// point later work at a live city's Dolt — and cmd/gc holds a standing ratchet
// against growing process-environment mutation in its test source (TESTING.md,
// "Small debt ratchet"). Asserting against the returned map keeps both
// properties without weakening the assertions: the export writes through
// ambientDoltEnvSet either way.
func fakeAmbientDoltEnv(t *testing.T, seed map[string]string) map[string]string {
	t.Helper()
	env := make(map[string]string, len(seed))
	for key, value := range seed {
		env[key] = value
	}
	previousGet, previousSet := ambientDoltEnvGet, ambientDoltEnvSet
	ambientDoltEnvGet = envGetterFromMap(env)
	ambientDoltEnvSet = func(key, value string) error { env[key] = value; return nil }
	t.Cleanup(func() { ambientDoltEnvGet, ambientDoltEnvSet = previousGet, previousSet })
	return env
}

func TestExportSupervisorAmbientDoltPortEnv(t *testing.T) {
	t.Run("disabled gate leaves the environment untouched", func(t *testing.T) {
		setSupervisorAmbientDoltPortExportForTest(t, false)
		env := fakeAmbientDoltEnv(t, nil)

		exportSupervisorAmbientDoltPortEnv("43307")

		if _, ok := env["GC_DOLT_PORT"]; ok {
			t.Error("GC_DOLT_PORT set despite disabled export gate")
		}
		if _, ok := env["BEADS_DOLT_SERVER_PORT"]; ok {
			t.Error("BEADS_DOLT_SERVER_PORT set despite disabled export gate")
		}
	})

	t.Run("enabled gate exports both port vars", func(t *testing.T) {
		setSupervisorAmbientDoltPortExportForTest(t, true)
		env := fakeAmbientDoltEnv(t, nil)

		exportSupervisorAmbientDoltPortEnv("43307")

		if got := env["GC_DOLT_PORT"]; got != "43307" {
			t.Errorf("GC_DOLT_PORT = %q, want %q", got, "43307")
		}
		if got := env["BEADS_DOLT_SERVER_PORT"]; got != "43307" {
			t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "43307")
		}
	})

	t.Run("port flip overwrites a stale ambient pin", func(t *testing.T) {
		// The systemd drop-in stopgap pinned a static port; every managed
		// Dolt respawn onto the sibling port then poisoned ambient
		// passthrough until an operator rebound the pin. The export must
		// overwrite, not respect, a stale value.
		setSupervisorAmbientDoltPortExportForTest(t, true)
		env := fakeAmbientDoltEnv(t, map[string]string{
			"GC_DOLT_PORT":           "29621",
			"BEADS_DOLT_SERVER_PORT": "29621",
		})

		exportSupervisorAmbientDoltPortEnv("29620")

		if got := env["GC_DOLT_PORT"]; got != "29620" {
			t.Errorf("GC_DOLT_PORT = %q, want %q", got, "29620")
		}
		if got := env["BEADS_DOLT_SERVER_PORT"]; got != "29620" {
			t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "29620")
		}
	})

	t.Run("blank port never unsets the last known-good hint", func(t *testing.T) {
		setSupervisorAmbientDoltPortExportForTest(t, true)
		env := fakeAmbientDoltEnv(t, map[string]string{
			"GC_DOLT_PORT":           "43307",
			"BEADS_DOLT_SERVER_PORT": "43307",
		})

		exportSupervisorAmbientDoltPortEnv("   ")

		if got := env["GC_DOLT_PORT"]; got != "43307" {
			t.Errorf("GC_DOLT_PORT = %q, want preserved %q", got, "43307")
		}
		if got := env["BEADS_DOLT_SERVER_PORT"]; got != "43307" {
			t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want preserved %q", got, "43307")
		}
	})
}

// TestExportSupervisorAmbientDoltPortEnvSkipsRedundantWrites covers the
// dedup early return: when both keys already hold the target port the export
// must not write at all. Counting seam writes is what makes that observable —
// the resulting map is identical either way, so a value assertion cannot tell
// a skipped write from a repeated one. That guard is what keeps two os.Setenv
// calls off every supervisor reconcile tick.
func TestExportSupervisorAmbientDoltPortEnvSkipsRedundantWrites(t *testing.T) {
	setSupervisorAmbientDoltPortExportForTest(t, true)
	env := fakeAmbientDoltEnv(t, nil)
	writes := 0
	previousSet := ambientDoltEnvSet
	ambientDoltEnvSet = func(key, value string) error {
		writes++
		return previousSet(key, value)
	}
	t.Cleanup(func() { ambientDoltEnvSet = previousSet })

	exportSupervisorAmbientDoltPortEnv("43307")
	if writes != 2 {
		t.Fatalf("first export made %d seam writes, want 2 (GC_DOLT_PORT + BEADS_DOLT_SERVER_PORT)", writes)
	}

	exportSupervisorAmbientDoltPortEnv("43307")
	if writes != 2 {
		t.Errorf("re-export of the same port made %d total seam writes, want 2 (no rewrite)", writes)
	}
	if env[envDoltPort] != "43307" || env[envBeadsDoltServerPort] != "43307" {
		t.Errorf("seam writes did not land: %v", env)
	}
}
