package main

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// sessionDoltPortKeys are the env keys a raw bd in an agent session reads
// for its Dolt port, ahead of the scope's .beads/dolt-server.port mirror.
var sessionDoltPortKeys = []string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

// mirroredManagedDoltCity builds a managed-direct city whose live runtime
// state publishes a reachable port, plus an inherited_city rig, and lets gc's
// own publication sync write both .beads/dolt-server.port mirrors. It returns
// the city path, the rig root and the rig list.
func mirroredManagedDoltCity(t *testing.T) (string, string, []config.Rig) {
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
	return cityPath, rigRoot, rigs
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
	cityPath, _, rigs := mirroredManagedDoltCity(t)
	setStaleSessionDoltPortEnv(t)

	env := mustSessionBackendEnv(t, cityPath, "", rigs)

	assertSessionDoltPortWithheld(t, env)
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Errorf("BEADS_DOLT_AUTO_START = %q, want 0", got)
	}
}

func TestSessionBackendEnvWithholdsMirroredInheritedRigDoltPort(t *testing.T) {
	cityPath, rigRoot, rigs := mirroredManagedDoltCity(t)
	setStaleSessionDoltPortEnv(t)

	env := mustSessionBackendEnv(t, cityPath, rigRoot, rigs)

	assertSessionDoltPortWithheld(t, env)
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Errorf("GC_DOLT_HOST = %q, want empty for a managed target", got)
	}
}

// With no env port, a user-level dolt.shared-server or an inherited
// BEADS_DOLT_SHARED_SERVER=1 would send bd to ~/.beads/shared-server instead
// of the scope's own mirror, so a withheld session pins the mode off.
func TestSessionBackendEnvWithheldPortPinsSharedServerOff(t *testing.T) {
	cityPath, rigRoot, rigs := mirroredManagedDoltCity(t)
	setStaleSessionDoltPortEnv(t)
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")

	for _, scope := range []struct{ name, rigRoot string }{{"city", ""}, {"rig", rigRoot}} {
		t.Run(scope.name, func(t *testing.T) {
			env := mustSessionBackendEnv(t, cityPath, scope.rigRoot, rigs)

			assertSessionDoltPortWithheld(t, env)
			assertSharedServerPinnedOff(t, scope.name+" session", env)
			if got := env["BEADS_DOLT_SHARED_SERVER"]; got != "0" {
				t.Errorf("BEADS_DOLT_SHARED_SERVER = %q, want 0", got)
			}
		})
	}
}

// Wherever the scope mirror cannot stand in for the env port, the session
// keeps the projection it has always had: the guard rows each break exactly
// one withholding condition.
func TestSessionBackendEnvKeepsDoltPortWhenMirrorCannotStandIn(t *testing.T) {
	writeRigConfig := func(t *testing.T, rigRoot, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMirror := func(t *testing.T, scopeRoot, port string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "dolt-server.port"), []byte(port+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		// setup adjusts the mirrored fixture and returns the session's rig
		// root ("" for a city session), its rigs, and the projected port
		// and host the session must keep.
		setup func(t *testing.T, cityPath, rigRoot string, rigs []config.Rig) (string, []config.Rig, string, string)
	}{
		{
			name: "city mirror absent",
			setup: func(t *testing.T, cityPath, _ string, rigs []config.Rig) (string, []config.Rig, string, string) {
				port := mustReadScopeDoltPortMirror(t, cityPath)
				if err := os.Remove(filepath.Join(cityPath, ".beads", "dolt-server.port")); err != nil {
					t.Fatal(err)
				}
				return "", rigs, port, ""
			},
		},
		{
			name: "rig mirror reads a different port",
			setup: func(t *testing.T, cityPath, rigRoot string, rigs []config.Rig) (string, []config.Rig, string, string) {
				port := mustReadScopeDoltPortMirror(t, cityPath)
				writeMirror(t, rigRoot, "31364")
				return rigRoot, rigs, port, ""
			},
		},
		{
			name: "legacy port-only rig",
			setup: func(t *testing.T, _, rigRoot string, _ []config.Rig) (string, []config.Rig, string, string) {
				writeRigConfig(t, rigRoot, "issue_prefix: repo\ndolt.auto-start: false\n")
				writeMirror(t, rigRoot, "4408")
				return rigRoot, []config.Rig{{Name: "repo", Path: rigRoot, DoltPort: "4408"}}, "4408", "127.0.0.1"
			},
		},
		{
			name: "explicit loopback rig target",
			setup: func(t *testing.T, _, rigRoot string, rigs []config.Rig) (string, []config.Rig, string, string) {
				writeRigConfig(t, rigRoot, "issue_prefix: repo\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.auto-start: false\ndolt.host: 127.0.0.1\ndolt.port: 4409\n")
				writeMirror(t, rigRoot, "4409")
				return rigRoot, rigs, "4409", "127.0.0.1"
			},
		},
		{
			name: "socket rig target",
			setup: func(t *testing.T, _, rigRoot string, rigs []config.Rig) (string, []config.Rig, string, string) {
				writeRigConfig(t, rigRoot, "issue_prefix: repo\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.auto-start: false\ndolt.socket: /var/run/dolt/repo.sock\n")
				return rigRoot, rigs, "", ""
			},
		},
		{
			name: "non-loopback rig host",
			setup: func(t *testing.T, _, rigRoot string, rigs []config.Rig) (string, []config.Rig, string, string) {
				writeRigConfig(t, rigRoot, "issue_prefix: repo\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.auto-start: false\ndolt.host: rig-db.example.com\ndolt.port: 4410\n")
				writeMirror(t, rigRoot, "4410")
				return rigRoot, rigs, "4410", "rig-db.example.com"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, rigRoot, rigs := mirroredManagedDoltCity(t)
			setStaleSessionDoltPortEnv(t)
			scopeRig, scopeRigs, wantPort, wantHost := tc.setup(t, cityPath, rigRoot, rigs)

			env := mustSessionBackendEnv(t, cityPath, scopeRig, scopeRigs)

			for _, key := range []string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT"} {
				if got := env[key]; got != wantPort {
					t.Errorf("%s = %q, want projected %q", key, got, wantPort)
				}
			}
			if got := env["GC_DOLT_HOST"]; got != wantHost {
				t.Errorf("GC_DOLT_HOST = %q, want %q", got, wantHost)
			}
			for _, key := range []string{"BEADS_DOLT_PORT", "BEADS_DOLT_SHARED_SERVER", "BD_DOLT_SHARED_SERVER"} {
				if got, present := env[key]; present {
					t.Errorf("%s = %q, want it left unprojected", key, got)
				}
			}
		})
	}
}

// Proxied, storage-bound and doltlite scopes keep their projection: a scope
// mirror matching whatever port they carry changes nothing, and no withheld
// key or extra shared-server opt-out appears.
func TestSessionBackendEnvLeavesUnmirroredTopologiesUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup returns the city path, the session's rig root ("" for a
		// city session) and the scope whose mirror bd would read.
		setup func(t *testing.T) (cityPath, rigRoot, scopeRoot string)
		// sharedServerPinned is the base projection's shared-server opt-out
		// for the scope.
		sharedServerPinned bool
	}{
		{
			name: "gc-owned proxied city",
			setup: func(t *testing.T) (string, string, string) {
				cityPath, _ := proxiedEnvTestCity(t)
				journalProxiedScope(t, cityPath, cityPath, true)
				return cityPath, "", cityPath
			},
			sharedServerPinned: true,
		},
		{
			name: "gc-owned proxied rig",
			setup: func(t *testing.T) (string, string, string) {
				cityPath, _ := proxiedEnvTestCity(t)
				rig := filepath.Join(cityPath, "rigs", "r1")
				writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`)
				journalProxiedScope(t, cityPath, rig, true)
				return cityPath, rig, rig
			},
			sharedServerPinned: true,
		},
		{
			name: "found proxied city",
			setup: func(t *testing.T) (string, string, string) {
				cityPath, _ := proxiedEnvTestCity(t)
				return cityPath, "", cityPath
			},
		},
		{
			name: "storage-bound city",
			setup: func(t *testing.T) (string, string, string) {
				cityPath := writeLibraryBackendScope(t)
				return cityPath, "", cityPath
			},
		},
		{
			name: "doltlite city",
			setup: func(t *testing.T) (string, string, string) {
				cityPath := t.TempDir()
				writeScopeBeadsMetadata(t, cityPath, `{"database":"doltlite","backend":"doltlite","dolt_database":"hq"}`)
				writeScopeConfigYAML(t, cityPath, "issue_prefix: hq\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n")
				return cityPath, "", cityPath
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setStaleSessionDoltPortEnv(t)
			t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
			cityPath, rigRoot, scopeRoot := tc.setup(t)

			projected, projectedErr := sessionBackendEnvWithError(cityPath, rigRoot, nil)
			if port := projected["GC_DOLT_PORT"]; port != "" {
				if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "dolt-server.port"), []byte(port+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			mirrored, mirroredErr := sessionBackendEnvWithError(cityPath, rigRoot, nil)

			if (projectedErr == nil) != (mirroredErr == nil) {
				t.Fatalf("error changed with the mirror: %v -> %v", projectedErr, mirroredErr)
			}
			if !maps.Equal(projected, mirrored) {
				t.Errorf("session env changed with a matching mirror:\n before %v\n after  %v", projected, mirrored)
			}
			if got, present := mirrored["BEADS_DOLT_PORT"]; present {
				t.Errorf("BEADS_DOLT_PORT = %q, want it left unprojected", got)
			}
			if tc.sharedServerPinned {
				assertSharedServerPinnedOff(t, tc.name, mirrored)
				return
			}
			for _, key := range []string{"BEADS_DOLT_SHARED_SERVER", "BD_DOLT_SHARED_SERVER"} {
				if got, present := mirrored[key]; present {
					t.Errorf("%s = %q, want no shared-server opt-out", key, got)
				}
			}
		})
	}
}

// Withholding changes only keys the fingerprint allow-list ignores, so a
// session spawned with the port projected is not drained once the port is
// withheld, nor when the managed Dolt later moves.
func TestSessionBackendEnvWithholdingKeepsConfigFingerprint(t *testing.T) {
	cityPath, _, rigs := mirroredManagedDoltCity(t)
	setStaleSessionDoltPortEnv(t)
	mirrorPath := filepath.Join(cityPath, ".beads", "dolt-server.port")
	mirror, err := os.ReadFile(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(mirrorPath); err != nil {
		t.Fatal(err)
	}
	projected := mustSessionBackendEnv(t, cityPath, "", rigs)
	if err := os.WriteFile(mirrorPath, mirror, 0o644); err != nil {
		t.Fatal(err)
	}
	withheld := mustSessionBackendEnv(t, cityPath, "", rigs)
	if projected["GC_DOLT_PORT"] == "" || withheld["GC_DOLT_PORT"] != "" {
		t.Fatalf("fixture: GC_DOLT_PORT projected %q, withheld %q; want a port then empty", projected["GC_DOLT_PORT"], withheld["GC_DOLT_PORT"])
	}

	session := func(env map[string]string) runtime.Config {
		env = maps.Clone(env)
		env["GC_CITY"] = cityPath
		env["GC_TEMPLATE"] = "worker"
		return runtime.Config{Command: "agent --serve", Env: env}
	}
	if got, want := runtime.CoreFingerprint(session(withheld)), runtime.CoreFingerprint(session(projected)); got != want {
		t.Errorf("CoreFingerprint withheld = %s, projected = %s; withholding would drain the session", got, want)
	}
	if got, want := runtime.ConfigFingerprint(session(withheld)), runtime.ConfigFingerprint(session(projected)); got != want {
		t.Errorf("ConfigFingerprint withheld = %s, projected = %s; withholding would drain the session", got, want)
	}
}
