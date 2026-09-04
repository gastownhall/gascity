package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestConfigStateConstructorsSelectDoltModes verifies that fresh managed
// scopes use the direct server path by default, while an explicit
// mode=proxied-server opts into Beads' proxied-server path. External endpoints
// remain direct unless paired with an explicit proxied mode. Existing authoritative scope state is resolved by
// resolveDesired*EndpointState before these constructors are used, so changing
// the default does not migrate an already initialized scope implicitly.
func TestConfigStateConstructorsSelectDoltModes(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rig")

	// Fresh managed city defaults to Beads' proxied-local lifecycle.
	managedCity := desiredCityDoltConfigState(cityPath, config.DoltConfig{}, "gc")
	if managedCity.DoltMode != "proxied-server" {
		t.Errorf("desiredCityDoltConfigState (managed city): DoltMode = %q, want %q", managedCity.DoltMode, "proxied-server")
	}
	proxyCity := desiredCityDoltConfigState(cityPath, config.DoltConfig{Mode: "proxied-server"}, "gc")
	if proxyCity.DoltMode != "proxied-server" {
		t.Errorf("desiredCityDoltConfigState (explicit proxy): DoltMode = %q, want %q", proxyCity.DoltMode, "proxied-server")
	}

	// External city (explicit host/port endpoint).
	externalCity := desiredCityDoltConfigState(cityPath, config.DoltConfig{Host: "db.example.com", Port: 3306}, "gc")
	if externalCity.DoltMode != "server" {
		t.Errorf("desiredCityDoltConfigState (external city): DoltMode = %q, want %q", externalCity.DoltMode, "server")
	}

	// Explicit rig (own dolt host/port override).
	explicitRig := desiredRigDoltConfigState(cityPath, config.Rig{
		Name: "rig", Path: rigPath, Prefix: "rig", DoltHost: "db.example.com", DoltPort: "3306",
	}, managedCity)
	if explicitRig.DoltMode != "server" {
		t.Errorf("desiredRigDoltConfigState (explicit rig): DoltMode = %q, want %q", explicitRig.DoltMode, "server")
	}

	// An inherited rig propagates the city's selected mode.
	inheritedRig := inheritedRigDoltConfigState(rigPath, "rig", managedCity)
	if inheritedRig.DoltMode != managedCity.DoltMode {
		t.Errorf("inheritedRigDoltConfigState: DoltMode = %q, want %q (inherited from city)", inheritedRig.DoltMode, managedCity.DoltMode)
	}

	// Requested rig endpoint (self path — gc manages this rig's Dolt).
	selfEndpoint := requestedRigEndpointState(
		config.Rig{Name: "rig", Path: rigPath, Prefix: "rig"},
		contract.ConfigState{}, managedCity,
		rigEndpointOptions{Self: true, Port: "3306"},
	)
	if selfEndpoint.DoltMode != "server" {
		t.Errorf("requestedRigEndpointState (self): DoltMode = %q, want %q", selfEndpoint.DoltMode, "server")
	}

	// Requested rig endpoint (explicit host/port path).
	externalEndpoint := requestedRigEndpointState(
		config.Rig{Name: "rig", Path: rigPath, Prefix: "rig"},
		contract.ConfigState{}, managedCity,
		rigEndpointOptions{Host: "db.example.com", Port: "3306", User: "bd"},
	)
	if externalEndpoint.DoltMode != "server" {
		t.Errorf("requestedRigEndpointState (external): DoltMode = %q, want %q", externalEndpoint.DoltMode, "server")
	}
}

// TestResolveDesiredCityEndpointStatePreservesAuthoritativeDoltMode ensures
// changing the fresh-scope default does not rewrite an existing workspace's
// mode. The resolver must return the persisted direct-server (or proxied)
// marker whenever canonical endpoint authority is already present.
func TestResolveDesiredCityEndpointStatePreservesAuthoritativeDoltMode(t *testing.T) {
	for _, mode := range []string{"server", "proxied-server"} {
		t.Run(mode, func(t *testing.T) {
			cityPath := t.TempDir()
			beadsDir := filepath.Join(cityPath, ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			configYAML := "issue_prefix: gc\n" +
				"gc.endpoint_origin: managed_city\n" +
				"gc.endpoint_status: verified\n" +
				"dolt.mode: " + mode + "\n"
			if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
				t.Fatal(err)
			}

			state, authoritative, err := resolveDesiredCityEndpointState(cityPath, config.DoltConfig{}, "gc")
			if err != nil {
				t.Fatalf("resolveDesiredCityEndpointState: %v", err)
			}
			if !authoritative {
				t.Fatal("resolver reported existing authoritative config as non-authoritative")
			}
			if state.DoltMode != mode {
				t.Fatalf("DoltMode = %q, want persisted %q", state.DoltMode, mode)
			}
		})
	}
}

func TestCanonicalBdScopeInitPersistsConfiguredDoltMode(t *testing.T) {
	for _, tc := range []struct {
		name, doltSection, mode, want string
	}{
		{name: "proxied default", want: "proxied-server"},
		{name: "explicit proxy", doltSection: "[dolt]\nmode = \"proxied-server\"\n", mode: "proxied-server", want: "proxied-server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"+tc.doltSection), 0o644); err != nil {
				t.Fatal(err)
			}
			state := desiredCityDoltConfigState(cityPath, config.DoltConfig{Mode: tc.mode}, "gc")
			if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, cityPath, state); err != nil {
				t.Fatalf("ensureCanonicalScopeConfigState: %v", err)
			}
			if err := ensureCanonicalScopeMetadata(fsys.OSFS{}, cityPath, "hq", false); err != nil {
				t.Fatalf("ensureCanonicalScopeMetadata: %v", err)
			}
			mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "metadata.json"))
			if err != nil || !ok {
				t.Fatalf("ReadDoltMode: mode=%q ok=%v err=%v", mode, ok, err)
			}
			if mode != tc.want {
				t.Fatalf("metadata dolt_mode = %q, want %q", mode, tc.want)
			}
		})
	}
}

func TestScopeUsesProxiedDoltModePersistedMetadataWinsOverConfig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"server"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.mode: proxied-server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scopeUsesProxiedDoltMode(cityPath, cityPath); got {
		t.Fatal("persisted direct metadata was overridden by proxied config selection")
	}
}

// A doltlite scope may retain a stale dolt_mode marker from an older
// canonicalisation.  The marker belongs to the Dolt backend only; it must not
// route the scope through beads' proxied-Dolt lifecycle.
func TestScopeUsesProxiedDoltModeRejectsStaleDoltliteMetadata(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n[beads]\nprovider = \"bd\"\nbackend = \"doltlite\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"backend":"doltlite","dolt_mode":"proxied-server"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := scopeUsesProxiedDoltMode(cityPath, cityPath); got {
		t.Fatal("scopeUsesProxiedDoltMode classified doltlite metadata as proxied-server")
	}
}

// A stale config marker is subject to the same backend gate when metadata
// carries the effective doltlite backend.
func TestScopeUsesProxiedDoltModeRejectsStaleDoltliteConfig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n[beads]\nprovider = \"bd\"\nbackend = \"doltlite\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"backend":"doltlite"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.mode: proxied-server\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := scopeUsesProxiedDoltMode(cityPath, cityPath); got {
		t.Fatal("scopeUsesProxiedDoltMode classified doltlite config as proxied-server")
	}
}
