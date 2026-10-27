package main

import (
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeManagedDirectCityFixture builds the canonical managed-direct city the
// port-withhold path targets: a bd provider, server-mode Dolt metadata, a
// reachable managed runtime state, and the .beads/dolt-server.port mirror
// syncConfiguredDoltPortFiles reconciles to the live port. It returns the
// live port, the one both the runtime state and the mirror carry.
func writeManagedDirectCityFixture(t *testing.T, cityPath string) string {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	writeScopeMetadata(t, cityPath, map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": "hq",
	})
	port := strconv.Itoa(writeReachableManagedDoltState(t, cityPath))
	requireSyncConfiguredDoltPortFiles(t, cityPath, "bd", config.DoltConfig{}, "gc", nil)
	return port
}

// assertWithheldDoltPort checks the withheld-port contract of
// gastownhall/gascity#1656: the three port keys stay present but empty, so
// tmux/subprocess providers unset any stale inherited value instead of
// freezing the spawn-time port into the session for its whole life.
func assertWithheldDoltPort(t *testing.T, env map[string]string, livePort string) {
	t.Helper()
	for _, key := range []string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"} {
		value, projected := env[key]
		if !projected {
			t.Fatalf("%s absent from session env; want present and empty so providers unset a stale inherited value", key)
		}
		if value != "" {
			t.Fatalf("%s = %q, want withheld (empty): the scope mirror .beads/dolt-server.port carries the live port %s", key, value, livePort)
		}
	}
}

// TestSessionBackendEnvWithholdsMirroredCityManagedDoltPort pins AC-1 of
// gastownhall/gascity#1656: a city session whose scope mirror already carries
// the live managed port must not have that port frozen into its env. bd ranks
// the env port above the mirror, so a projected port keeps every session
// dialing the port dolt ran on at spawn time, long after a restart moved it;
// withholding the env port lets the session's raw `bd` follow the mirror gc
// rewrites on every runtime publication.
func TestSessionBackendEnvWithholdsMirroredCityManagedDoltPort(t *testing.T) {
	cityPath := t.TempDir()
	livePort := writeManagedDirectCityFixture(t, cityPath)
	// Stale spawn-time values, as a session environment captured before a
	// Dolt restart would carry them.
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	env, err := sessionBackendEnvWithError(cityPath, "", nil)
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError: %v", err)
	}

	assertWithheldDoltPort(t, env, livePort)
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Fatalf("BEADS_DOLT_AUTO_START = %q, want 0", got)
	}
}
