package main

import (
	"os"
	"path/filepath"
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

// TestSessionBackendEnvWithholdsMirroredInheritedRigDoltPort pins AC-2 of
// gastownhall/gascity#1656: a rig session under the canonical inherited-city
// contract must have the managed port withheld when the rig's own
// .beads/dolt-server.port mirror — the one syncConfiguredDoltPortFiles
// reconciles to the live port — already carries it, for the same reason as
// the city scope: bd ranks an env port above the mirror, so projecting it
// freezes the spawn-time port into the rig session across a managed Dolt
// restart onto a new port.
func TestSessionBackendEnvWithholdsMirroredInheritedRigDoltPort(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	rigRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	// The rig contract gc publishes for an inherited-city rig — endpoint
	// origin inherited_city, no pinned port — with the port mirror carrying
	// the live managed port, as syncConfiguredDoltPortFiles reconciles it.
	livePort := strconv.Itoa(writeReachableManagedDoltState(t, cityPath))
	if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "dolt-server.port"), []byte(livePort), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stale spawn-time values, as a session environment captured before a
	// Dolt restart would carry them.
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	env, err := sessionBackendEnvWithError(cityPath, rigRoot, []config.Rig{{Name: "repo", Path: rigRoot}})
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError: %v", err)
	}

	assertWithheldDoltPort(t, env, livePort)
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for the managed loopback target", got)
	}
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Fatalf("BEADS_DOLT_AUTO_START = %q, want 0", got)
	}
}

// TestSessionBackendEnvWithheldPortPinsSharedServerOff pins AC-4 of
// gastownhall/gascity#1656: withholding the env port removes the one fact
// that kept bd off a shared server, so a user-level `dolt.shared-server`
// (or an inherited BEADS_DOLT_SHARED_SERVER=1) would now send the
// session's raw bd to ~/.beads/shared-server or port 3308 instead of the
// managed server the mirror tracks. The withhold must pin the mode off
// explicitly, the same split-brain rule applySessionSharedServerOptOut
// applies to proxied gc-owned scopes.
func TestSessionBackendEnvWithheldPortPinsSharedServerOff(t *testing.T) {
	cityPath := t.TempDir()
	livePort := writeManagedDirectCityFixture(t, cityPath)
	// An inherited shared-server preference, as a session spawned from a
	// process carrying it would pass down.
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")

	env, err := sessionBackendEnvWithError(cityPath, "", nil)
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError: %v", err)
	}

	assertWithheldDoltPort(t, env, livePort)
	if got := env["BEADS_DOLT_SHARED_SERVER"]; got != "0" {
		t.Fatalf("BEADS_DOLT_SHARED_SERVER = %q, want 0 pinned off beside a withheld port", got)
	}
	if got := env["BD_DOLT_SHARED_SERVER"]; got != "false" {
		t.Fatalf("BD_DOLT_SHARED_SERVER = %q, want false pinned off beside a withheld port", got)
	}
}
