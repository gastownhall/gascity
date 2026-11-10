package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
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

func journalProxiedScope(t *testing.T, cityPath, scopeRoot string, ready bool) {
	t.Helper()
	if err := persistProviderScopeOwnership(cityPath, scopeRoot, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if ready {
		if err := markProviderScopeOwnershipReady(cityPath, scopeRoot); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProxiedRuntimeEnvPinsBdSharedServerOff(t *testing.T) {
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	cityPath, _ := proxiedEnvTestCity(t)
	rig := filepath.Join(cityPath, "rigs", "r1")
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`)
	journalProxiedScope(t, cityPath, cityPath, true)
	journalProxiedScope(t, cityPath, rig, true)

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
}

// The provider script's process env is city-wide (it serves rig ops too), so it
// carries the opt-out only while a journaled proxied init is in flight — gc-owned
// by construction. A ready scope is pinned by the script itself, per scope, from
// its config.yaml pin.
func TestProviderLifecycleEnvPinsSharedServerOffOnlyForInitializingProxiedCity(t *testing.T) {
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	cityPath, _ := proxiedEnvTestCity(t)
	journalProxiedScope(t, cityPath, cityPath, false)
	provEnv, err := providerLifecycleProcessEnvFromBase(cityPath, beadsProvider(cityPath), os.Environ())
	if err != nil {
		t.Fatalf("providerLifecycleProcessEnvFromBase: %v", err)
	}
	assertSharedServerPinnedOff(t, "initializing provider lifecycle", runtimeEnvEntriesToMap(provEnv))

	if err := markProviderScopeOwnershipReady(cityPath, cityPath); err != nil {
		t.Fatal(err)
	}
	provEnv, err = providerLifecycleProcessEnvFromBase(cityPath, beadsProvider(cityPath), os.Environ())
	if err != nil {
		t.Fatalf("providerLifecycleProcessEnvFromBase: %v", err)
	}
	m := runtimeEnvEntriesToMap(provEnv)
	if m["BD_DOLT_SHARED_SERVER"] == "false" || m["BEADS_DOLT_SHARED_SERVER"] != "1" {
		t.Errorf("ready provider lifecycle env was pinned city-wide: BD=%q BEADS=%q", m["BD_DOLT_SHARED_SERVER"], m["BEADS_DOLT_SHARED_SERVER"])
	}
}

// A proxied workspace gc merely found (no journal, handoff or endpoint marker)
// whose config says shared-server: true must be resolved the SAME way by gc's
// own bd and by an agent's shell bd: gc must not pin its runners off while the
// agent follows the scope config into the shared server.
func TestFoundProxiedScopeGetsNoSharedServerOptOutAnywhere(t *testing.T) {
	cityPath, _ := proxiedEnvTestCity(t)
	const body = "dolt:\n  shared-server: true\n"
	writeScopeConfigYAML(t, cityPath, body)

	runtimeEnv, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError: %v", err)
	}
	sessionEnv, _ := sessionBackendEnvWithError(cityPath, "", nil)
	for label, env := range map[string]map[string]string{"gc runtime": runtimeEnv, "agent session": sessionEnv} {
		for _, key := range []string{"BD_DOLT_SHARED_SERVER", "BEADS_DOLT_SHARED_SERVER"} {
			if value, projected := env[key]; projected {
				t.Errorf("%s env for a found proxied scope projects %s=%q; gc and agent bd would disagree", label, key, value)
			}
		}
	}
	if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, cityPath); err != nil {
		t.Fatal(err)
	}
	if pin := readScopeSharedServerPin(t, cityPath); pin != contract.SharedServerPinnedOn {
		t.Fatalf("found scope config rewritten: pin = %v", pin)
	}
}

// The opt-out is a statement about scopes gc owns. A rig gc does not own (here,
// one on an external host) must not inherit it from its proxied city's
// projection: its bd resolution (and the operator's shared-server choice for
// it) is unchanged.
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
// running `bd` in its shell — so it has to land in every scope gc owns, proxied
// or managed server-mode, and nowhere else.
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

	t.Run("a managed server-mode scope is pinned", func(t *testing.T) {
		cityPath := t.TempDir()
		writeScopeBeadsMetadata(t, cityPath, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
		writeScopeConfigYAML(t, cityPath, "gc.endpoint_origin: managed_city\n")
		if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, cityPath); err != nil {
			t.Fatal(err)
		}
		if pin := readScopeSharedServerPin(t, cityPath); pin != contract.SharedServerPinnedOff {
			t.Fatalf("pin = %v, want pinned off", pin)
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

// An owned scope initialized by a build that wrote no pin (the RC) must be
// pinned BEFORE any provider op on it runs bd: the ready scope's `start` op is a
// `bd ping`, and under a user-level shared-server: true an unpinned ping roots a
// proxy and Dolt child in ~/.beads/shared-server. Drives the real start path for
// an owned city and an owned rig.
func TestStartPinsOwnedScopesBeforeTheirFirstProviderOp(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	logPath := filepath.Join(city, "provider-ops")
	script := filepath.Join(city, "provider.sh")
	body := "#!/bin/sh\npin=no\ngrep -q 'shared-server: false' \"$BEADS_DIR/config.yaml\" 2>/dev/null && pin=yes\nprintf '%s %s cfgpin=%s\\n' \"$1\" \"$BEADS_DIR\" \"$pin\" >> \"$GC_TEST_PROVIDER_LOG\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n\n[[rigs]]\nname = \"r1\"\npath = \""+rig+"\"\nprefix = \"r1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for scope, db := range map[string]string{city: "hq", rig: "r1"} {
		writeScopeBeadsMetadata(t, scope, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"`+db+`"}`)
		writeScopeConfigYAML(t, scope, "issue_prefix: "+db+"\n")
		journalProxiedScope(t, city, scope, true)
	}
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := startBeadsLifecycle(city, "", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	sawRig := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, filepath.Join(rig, ".beads")) {
			sawRig = true
		}
		if !strings.HasSuffix(line, "cfgpin=yes") {
			t.Errorf("provider op ran on an owned scope before its pin was written: %s", line)
		}
	}
	if !sawRig {
		t.Fatalf("no provider op ran for the rig:\n%s", data)
	}
}

// A proxied rig gc merely found, under a gc-owned city, must not inherit the
// city's opt-out: gc's bd for the rig and an agent's bd in the rig agree.
func TestFoundProxiedRigUnderOwnedCityAgreesWithAgentEnv(t *testing.T) {
	cityPath, _ := proxiedEnvTestCity(t)
	journalProxiedScope(t, cityPath, cityPath, true)
	rig := filepath.Join(cityPath, "rigs", "found")
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"found"}`)
	const body = "dolt:\n  shared-server: true\n"
	writeScopeConfigYAML(t, rig, body)

	cityEnv, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError: %v", err)
	}
	assertSharedServerPinnedOff(t, "owned city", cityEnv)

	rigEnv, err := bdRuntimeEnvForRigWithError(cityPath, nil, rig)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError: %v", err)
	}
	sessionEnv, _ := sessionBackendEnvWithError(cityPath, rig, nil)
	for label, env := range map[string]map[string]string{"gc rig runtime": rigEnv, "agent rig session": sessionEnv} {
		for _, key := range []string{"BD_DOLT_SHARED_SERVER", "BEADS_DOLT_SHARED_SERVER"} {
			if value, projected := env[key]; projected {
				t.Errorf("%s env for a found proxied rig projects %s=%q; gc and agent bd would disagree", label, key, value)
			}
		}
	}
	if err := ensureGCOwnedProxiedScopeSharedServerOff(cityPath, rig); err != nil {
		t.Fatal(err)
	}
	if pin := readScopeSharedServerPin(t, rig); pin != contract.SharedServerPinnedOn {
		t.Fatalf("found rig config rewritten: pin = %v", pin)
	}
}
