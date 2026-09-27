package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// F9: a user-level `dolt.shared-server: true` (~/.beads/config.yaml or
// ~/.config/bd/config.yaml) silently rooted gc-owned proxied scopes in
// ~/.beads/shared-server, where two cities' hq stores became one database.
// Every bd process gc spawns for a proxied scope must pin the mode off, and the
// pin must survive an ambient BEADS_DOLT_SHARED_SERVER=1 in gc's own process
// (the runners layer the projection over the inherited environment).
func assertSharedServerPinnedOff(t *testing.T, label string, env map[string]string) {
	t.Helper()
	if got := env["BD_DOLT_SHARED_SERVER"]; got != "false" {
		t.Errorf("%s: BD_DOLT_SHARED_SERVER = %q, want false", label, got)
	}
	got, projected := env["BEADS_DOLT_SHARED_SERVER"]
	if !projected || got == "1" || got == "true" {
		t.Errorf("%s: BEADS_DOLT_SHARED_SERVER = %q (projected=%v), want an explicit non-true value that overrides the ambient 1", label, got, projected)
	}
}

func TestProxiedRuntimeEnvPinsBdSharedServerOff(t *testing.T) {
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	cityPath, _ := proxiedEnvTestCity(t)
	rig := filepath.Join(cityPath, "rigs", "r1")
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`)

	cityEnv, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError: %v", err)
	}
	assertSharedServerPinnedOff(t, "city", cityEnv)

	rigEnv, err := bdRuntimeEnvForRigWithError(cityPath, nil, rig)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError: %v", err)
	}
	assertSharedServerPinnedOff(t, "rig", rigEnv)

	// The provider script's own process env (init, start, health, stop run
	// bd from there) takes the same projection.
	provEnv, err := providerLifecycleProcessEnvFromBase(cityPath, beadsProvider(cityPath), os.Environ())
	if err != nil {
		t.Fatalf("providerLifecycleProcessEnvFromBase: %v", err)
	}
	assertSharedServerPinnedOff(t, "provider lifecycle", runtimeEnvEntriesToMap(provEnv))
}

// The opt-out is a statement about proxied scopes gc owns. A rig that is not
// proxied must not inherit it from its proxied city's projection: its bd
// resolution (and the operator's shared-server choice for it) is unchanged.
func TestNonProxiedRigDoesNotInheritTheCitySharedServerPin(t *testing.T) {
	cityPath, _ := proxiedEnvTestCity(t)
	rig := filepath.Join(cityPath, "rigs", "direct")
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"direct","dolt_server_host":"db.example.test","dolt_server_port":3307}`)

	env, _ := bdRuntimeEnvForRigWithError(cityPath, nil, rig)
	for _, key := range []string{"BD_DOLT_SHARED_SERVER", "BEADS_DOLT_SHARED_SERVER"} {
		if value, projected := env[key]; projected {
			t.Errorf("non-proxied rig env projects %s=%q from the proxied city", key, value)
		}
	}
}

func readScopeSharedServerPin(t *testing.T, scopeRoot string) contract.SharedServerPin {
	t.Helper()
	pin, err := contract.ReadSharedServerPin(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

func writeScopeConfigYAML(t *testing.T, scopeRoot, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "config.yaml"), []byte(body), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
}

// The config.yaml pin covers the bd processes gc does not spawn — an agent
// running `bd` in its shell — so it has to land in every proxied scope gc owns,
// and nowhere else.
func TestEnsureGCOwnedProxiedScopeSharedServerOff(t *testing.T) {
	t.Run("journaled scope is pinned", func(t *testing.T) {
		cityPath, _ := proxiedEnvTestCity(t)
		if err := persistProviderScopeOwnership(cityPath, cityPath, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := markProviderScopeOwnershipReady(cityPath, cityPath); err != nil {
			t.Fatal(err)
		}
		writeScopeConfigYAML(t, cityPath, "# bd init template\n")
		if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, cityPath); err != nil {
			t.Fatal(err)
		}
		if pin := readScopeSharedServerPin(t, cityPath); pin != contract.SharedServerPinnedOff {
			t.Fatalf("pin = %v, want pinned off", pin)
		}
	})

	t.Run("still-initializing journaled scope is pinned", func(t *testing.T) {
		// bd init has just run and the journal has not been marked ready yet:
		// this is the moment the pin is first written.
		cityPath, _ := proxiedEnvTestCity(t)
		if err := persistProviderScopeOwnership(cityPath, cityPath, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, cityPath); err != nil {
			t.Fatal(err)
		}
		if pin := readScopeSharedServerPin(t, cityPath); pin != contract.SharedServerPinnedOff {
			t.Fatalf("pin = %v, want pinned off", pin)
		}
	})

	t.Run("legacy managed scope bound on by an earlier init is flipped off", func(t *testing.T) {
		cityPath, _ := proxiedEnvTestCity(t)
		writeScopeConfigYAML(t, cityPath, "issue_prefix: hq\ngc.endpoint_origin: managed_city\ndolt.shared-server: true\n")
		if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, cityPath); err != nil {
			t.Fatal(err)
		}
		if pin := readScopeSharedServerPin(t, cityPath); pin != contract.SharedServerPinnedOff {
			t.Fatalf("pin = %v, want pinned off", pin)
		}
	})

	t.Run("a proxied workspace gc merely found is left alone", func(t *testing.T) {
		cityPath, _ := proxiedEnvTestCity(t)
		const body = "# operator's own workspace\ndolt.shared-server: true\n"
		writeScopeConfigYAML(t, cityPath, body)
		if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, cityPath); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(cityPath, ".beads", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != body {
			t.Fatalf("gc rewrote a scope it does not own:\n%s", data)
		}
	})

	t.Run("a non-proxied scope is left alone", func(t *testing.T) {
		cityPath := t.TempDir()
		writeScopeBeadsMetadata(t, cityPath, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
		writeScopeConfigYAML(t, cityPath, "gc.endpoint_origin: managed_city\n")
		if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, cityPath); err != nil {
			t.Fatal(err)
		}
		if pin := readScopeSharedServerPin(t, cityPath); pin != contract.SharedServerUnset {
			t.Fatalf("pin = %v, want a direct scope untouched", pin)
		}
	})
}

// An agent session runs `bd` in its own shell, outside every gc runner. The
// scope's config.yaml pin covers it — unless the session inherits
// BEADS_DOLT_SHARED_SERVER=1, which bd reads before any config file. The
// session projection therefore carries the same opt-out for the gc-owned
// proxied scope the session works in, and nothing for one gc does not own.
func TestSessionEnvPinsBdSharedServerOffForGCOwnedProxiedScopes(t *testing.T) {
	t.Run("gc-owned proxied city", func(t *testing.T) {
		cityPath, _ := proxiedEnvTestCity(t)
		if err := persistProviderScopeOwnership(cityPath, cityPath, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := markProviderScopeOwnershipReady(cityPath, cityPath); err != nil {
			t.Fatal(err)
		}
		env, err := sessionBackendEnvWithError(cityPath, "", nil)
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		assertSharedServerPinnedOff(t, "city session", env)
	})

	t.Run("gc-owned proxied rig", func(t *testing.T) {
		cityPath, _ := proxiedEnvTestCity(t)
		rig := filepath.Join(cityPath, "rigs", "r1")
		writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`)
		if err := persistProviderScopeOwnership(cityPath, rig, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := markProviderScopeOwnershipReady(cityPath, rig); err != nil {
			t.Fatal(err)
		}
		env, err := sessionBackendEnvWithError(cityPath, rig, nil)
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		assertSharedServerPinnedOff(t, "rig session", env)
	})

	t.Run("proxied workspace gc merely found", func(t *testing.T) {
		cityPath, _ := proxiedEnvTestCity(t)
		env, _ := sessionBackendEnvWithError(cityPath, "", nil)
		for _, key := range []string{"BD_DOLT_SHARED_SERVER", "BEADS_DOLT_SHARED_SERVER"} {
			if value, projected := env[key]; projected {
				t.Errorf("session env for an unowned proxied scope projects %s=%q", key, value)
			}
		}
	})
}

// A journaled scope bd has not materialized yet has no config to pin; the pin
// must not conjure a .beads directory (the next init or start writes it).
func TestEnsureGCOwnedProxiedScopeSharedServerOffSkipsUnmaterializedScope(t *testing.T) {
	cityPath, _ := proxiedEnvTestCity(t)
	rig := filepath.Join(cityPath, "rigs", "fresh")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(cityPath, rig, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, rig); err != nil {
		t.Fatalf("ensureGCOwnedProxiedScopeSharedServerOff: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rig, ".beads")); !os.IsNotExist(err) {
		t.Fatalf("pin created %s/.beads (stat err %v)", rig, err)
	}
}
