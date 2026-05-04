package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func canonicalTestPath(path string) string {
	return testutil.CanonicalPath(path)
}

func assertSameTestPath(t *testing.T, got, want string) {
	t.Helper()
	testutil.AssertSamePath(t, got, want)
}

func shortSocketTempDir(t *testing.T, prefix string) string {
	t.Helper()
	return testutil.ShortTempDir(t, prefix)
}

// clearInheritedBeadsEnv prevents tests that explicitly write
// [beads]\nprovider = "file" from being silently overridden by an agent
// session's inherited GC_BEADS=bd, which would trigger gc-beads-bd.sh and
// leak an orphan dolt sql-server because test cleanup paths do not call
// shutdownBeadsProvider.
func clearInheritedBeadsEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GC_BEADS",
		"GC_DOLT",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
		"GC_BEADS_SCOPE_ROOT",
	} {
		t.Setenv(key, "")
	}
}

func cleanupManagedDoltTestCity(t *testing.T, cityPath string) {
	t.Helper()
	t.Cleanup(func() {
		tryStopController(cityPath, io.Discard)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if controllerAlive(cityPath) == 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if port := currentManagedDoltPort(cityPath); port != "" {
			if _, err := stopManagedDoltProcess(cityPath, port); err != nil {
				t.Logf("stopManagedDoltProcess(%s, %s): %v", cityPath, port, err)
			}
		}
		if err := shutdownBeadsProvider(cityPath); err != nil {
			t.Logf("shutdownBeadsProvider(%s): %v", cityPath, err)
		}
	})
}
