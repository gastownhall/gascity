package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// sessionDoltPortKeys are the env keys a raw bd in an agent session reads
// for its Dolt port, ahead of the scope's .beads/dolt-server.port mirror.
var sessionDoltPortKeys = []string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

// mirroredManagedDoltCity builds a managed-direct city whose live runtime
// state publishes a reachable port, plus an inherited_city rig, and lets gc's
// own publication sync write both .beads/dolt-server.port mirrors. It returns
// the city path, the rig root, the rig list and the published port.
func mirroredManagedDoltCity(t *testing.T) (string, string, []config.Rig, string) {
	t.Helper()
	cityPath := t.TempDir()
	writeScopeMetadata(t, cityPath, map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": "hq",
	})
	port := strconv.Itoa(writeReachableManagedDoltState(t, cityPath))

	rigRoot := filepath.Join(t.TempDir(), "repo")
	writeScopeMetadata(t, rigRoot, map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": "repo",
	})
	if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "config.yaml"), []byte("issue_prefix: repo\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rigs := []config.Rig{{Name: "repo", Path: rigRoot}}
	if err := syncConfiguredDoltPortFiles(cityPath, config.DoltConfig{}, "gc", rigs, io.Discard); err != nil {
		t.Fatalf("syncConfiguredDoltPortFiles: %v", err)
	}
	for _, scope := range []string{cityPath, rigRoot} {
		if got := mustReadScopeDoltPortMirror(t, scope); got != port {
			t.Fatalf("%s mirror = %q, want published port %q", scope, got, port)
		}
		if !bdScopeDoltIsGcManaged(cityPath, scope) {
			t.Fatalf("fixture scope %s is not gc-managed", scope)
		}
	}
	return cityPath, rigRoot, rigs, port
}

func mustReadScopeDoltPortMirror(t *testing.T, scopeRoot string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "dolt-server.port"))
	if err != nil {
		t.Fatalf("read %s mirror: %v", scopeRoot, err)
	}
	return strings.TrimSpace(string(data))
}

// setStaleSessionDoltPortEnv puts the ports a long-lived parent process
// captured before a Dolt restart into the process env.
func setStaleSessionDoltPortEnv(t *testing.T) {
	t.Helper()
	for _, key := range sessionDoltPortKeys {
		t.Setenv(key, "9999")
	}
}

func assertSessionDoltPortWithheld(t *testing.T, env map[string]string) {
	t.Helper()
	for _, key := range sessionDoltPortKeys {
		got, present := env[key]
		if !present || got != "" {
			t.Errorf("%s = %q (present=%t), want present and empty so bd follows the scope mirror", key, got, present)
		}
	}
}

func TestSessionBackendEnvWithholdsMirroredCityManagedDoltPort(t *testing.T) {
	cityPath, _, rigs, _ := mirroredManagedDoltCity(t)
	setStaleSessionDoltPortEnv(t)

	env := mustSessionBackendEnv(t, cityPath, "", rigs)

	assertSessionDoltPortWithheld(t, env)
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Errorf("BEADS_DOLT_AUTO_START = %q, want 0", got)
	}
}
