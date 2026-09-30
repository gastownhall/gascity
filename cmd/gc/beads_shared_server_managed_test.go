package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// gm-uasy78: a city and its rigs run as gc-managed Dolt server-mode scopes —
// gc starts the server and every scope's config.yaml carries gc's endpoint
// marker. #6697 pinned bd's user-level shared-server mode off only for proxied
// scopes, so a host ~/.beads/config.yaml with `dolt.shared-server: true` still
// put every bd call the controller made behind ~/.beads/shared-server's gate
// lock, and a test's exclusive `bd init` under the real HOME locked the
// controller out for ~40s at a time. The pin belongs to every scope gc owns,
// whatever its transport.
//
// These tests assert on the projected env map and on config.yaml because that
// is where the immunity is decided: bd resolves an env binding above every
// config layer, so a map carrying BD_DOLT_SHARED_SERVER=false cannot be
// outvoted by a user-level file, and the map does not read HOME at all. The
// real-bd leg (hold the shared-root gate exclusively, run bd under gc's env)
// needs a real bd and belongs to the acceptance tier.

const (
	managedServerCityConfig  = "issue_prefix: hq\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.auto-start: false\n"
	inheritedServerRigConfig = "issue_prefix: r1\ngc.endpoint_origin: inherited_city\n"
	explicitServerRigConfig  = "issue_prefix: ext\ngc.endpoint_origin: explicit\ndolt.host: db.example.test\ndolt.port: 3307\n"
	serverModeHQMetadata     = `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`
	serverModeRigMetadata    = `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"r1"}`
	userPinnedSharedOn       = "dolt.shared-server: true\n"
)

// managedServerTestCity builds a city whose hq scope is a gc-managed Dolt
// server-mode store, with one rig inheriting the city's endpoint.
func managedServerTestCity(t *testing.T) (cityPath, rig string) {
	t.Helper()
	cityPath, _ = proxiedEnvTestCity(t)
	writeScopeBeadsMetadata(t, cityPath, serverModeHQMetadata)
	writeScopeConfigYAML(t, cityPath, managedServerCityConfig)
	rig = filepath.Join(cityPath, "rigs", "r1")
	writeScopeBeadsMetadata(t, rig, serverModeRigMetadata)
	writeScopeConfigYAML(t, rig, inheritedServerRigConfig)
	return cityPath, rig
}

func assertNoSharedServerOptOut(t *testing.T, label string, env map[string]string) {
	t.Helper()
	for _, key := range []string{"BD_DOLT_SHARED_SERVER", "BEADS_DOLT_SHARED_SERVER"} {
		if value, projected := env[key]; projected {
			t.Errorf("%s projects %s=%q for a scope gc does not own", label, key, value)
		}
	}
}

// AC1: every bd process gc spawns for a managed server-mode scope — the
// controller's, `gc bd`'s, orders', hooks' — carries the opt-out. The tail
// error is ignored on purpose: no Dolt server runs in this fixture, and the
// projection is returned alongside it either way.
func TestManagedServerRuntimeEnvPinsBdSharedServerOff(t *testing.T) {
	cityPath, rig := managedServerTestCity(t)

	cityEnv, _ := bdRuntimeEnvWithErrorNoRecovery(cityPath)
	assertSharedServerPinnedOff(t, "managed server city", cityEnv)

	rigEnv, _ := bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, nil, rig)
	assertSharedServerPinnedOff(t, "managed server rig", rigEnv)
}

// AC1: an agent's shell bd reads BEADS_DOLT_SHARED_SERVER before any config
// file, so the session projection carries the same opt-out.
func TestManagedServerSessionEnvPinsBdSharedServerOff(t *testing.T) {
	cityPath, rig := managedServerTestCity(t)

	cityEnv, err := sessionBackendEnvWithError(cityPath, "", nil)
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError(city): %v", err)
	}
	assertSharedServerPinnedOff(t, "managed server city session", cityEnv)

	rigEnv, err := sessionBackendEnvWithError(cityPath, rig, nil)
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError(rig): %v", err)
	}
	assertSharedServerPinnedOff(t, "managed server rig session", rigEnv)
}

// AC2: the config.yaml pin is what an agent's shell bd resolves, and what the
// native library reads, so it lands in every managed server-mode scope — and a
// scope an earlier bd init bound ON under the user-level mode is flipped off.
func TestEnsureGCOwnedScopeSharedServerOffPinsManagedServerScopes(t *testing.T) {
	cityPath, rig := managedServerTestCity(t)

	for label, scope := range map[string]string{"city": cityPath, "rig": rig} {
		if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, scope); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if pin := readScopeSharedServerPin(t, scope); pin != contract.SharedServerPinnedOff {
			t.Errorf("%s pin = %v, want pinned off", label, pin)
		}
	}

	bound := filepath.Join(cityPath, "rigs", "bound")
	writeScopeBeadsMetadata(t, bound, serverModeRigMetadata)
	writeScopeConfigYAML(t, bound, inheritedServerRigConfig+userPinnedSharedOn)
	if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, bound); err != nil {
		t.Fatal(err)
	}
	if pin := readScopeSharedServerPin(t, bound); pin != contract.SharedServerPinnedOff {
		t.Errorf("bound-on rig pin = %v, want flipped off", pin)
	}
}

// AC3: a server-mode scope is gc's only when its config.yaml says so and its
// store is gc's own Dolt. Everything else keeps the operator's resolution:
// no env opt-out, and a config.yaml gc leaves byte-for-byte alone (the user's
// `dolt.shared-server: true` in it survives).
func TestUnownedServerScopesKeepBdSharedServerResolution(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
		config   string
	}{
		{"no gc endpoint marker", serverModeHQMetadata, "issue_prefix: hq\ndolt.mode: server\n"},
		{"explicit external endpoint", serverModeHQMetadata, explicitServerRigConfig},
		{"city canonical external endpoint", serverModeHQMetadata, "issue_prefix: hq\ngc.endpoint_origin: city_canonical\ndolt.host: db.example.test\ndolt.port: 3307\n"},
		{"doltlite store under a managed marker", `{"database":"doltlite","backend":"doltlite","dolt_database":"hq"}`, managedServerCityConfig},
		{"externally bound store under a managed marker", completeStorageBindingJSON, managedServerCityConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, _ := proxiedEnvTestCity(t)
			writeScopeBeadsMetadata(t, cityPath, tc.metadata)
			body := tc.config + userPinnedSharedOn
			writeScopeConfigYAML(t, cityPath, body)

			runtimeEnv, _ := bdRuntimeEnvWithErrorNoRecovery(cityPath)
			assertNoSharedServerOptOut(t, "gc runtime env", runtimeEnv)
			sessionEnv, _ := sessionBackendEnvWithError(cityPath, "", nil)
			assertNoSharedServerOptOut(t, "agent session env", sessionEnv)

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
	}
}

// AC3: the opt-out is a per-scope statement. A rig that points at an external
// Dolt must not inherit it from its managed city's projection — the city
// function projects it first and the rig function has to take it back off.
func TestExplicitEndpointRigDoesNotInheritManagedCityBdSharedServerPin(t *testing.T) {
	cityPath, _ := managedServerTestCity(t)
	rig := filepath.Join(cityPath, "rigs", "external")
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"ext"}`)
	body := explicitServerRigConfig + userPinnedSharedOn
	writeScopeConfigYAML(t, rig, body)

	rigEnv, _ := bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, nil, rig)
	assertNoSharedServerOptOut(t, "external rig runtime env", rigEnv)
	sessionEnv, _ := sessionBackendEnvWithError(cityPath, rig, nil)
	assertNoSharedServerOptOut(t, "external rig session env", sessionEnv)

	if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, rig); err != nil {
		t.Fatal(err)
	}
	if pin := readScopeSharedServerPin(t, rig); pin != contract.SharedServerPinnedOn {
		t.Errorf("external rig config rewritten: pin = %v", pin)
	}
}
