package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestConfigStateConstructorsSelectDoltModes verifies that fresh managed
// scopes default to Beads' proxied-local path, while an explicit mode=server
// opts into the direct SQL-server path. External endpoints remain direct
// unless paired with an explicit proxied mode. Existing authoritative scope
// state is resolved by resolveDesired*EndpointState before these constructors
// are used, so changing the default does not migrate an already initialized
// scope implicitly.
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
	directCity := desiredCityDoltConfigState(cityPath, config.DoltConfig{Mode: "server"}, "gc")
	if directCity.DoltMode != "server" {
		t.Errorf("desiredCityDoltConfigState (explicit direct opt-out): DoltMode = %q, want %q", directCity.DoltMode, "server")
	}

	// External city (explicit host/port endpoint).
	externalCity := desiredCityDoltConfigState(cityPath, config.DoltConfig{Host: "db.example.com", Port: 3306}, "gc")
	if externalCity.DoltMode != "server" {
		t.Errorf("desiredCityDoltConfigState (external city): DoltMode = %q, want %q", externalCity.DoltMode, "server")
	}
	proxiedExternalCity := desiredCityDoltConfigState(cityPath, config.DoltConfig{Host: "db.example.com", Port: 3306, Mode: "proxied-server"}, "gc")
	if proxiedExternalCity.DoltMode != "proxied-server" {
		t.Errorf("desiredCityDoltConfigState (proxied external city): DoltMode = %q, want %q", proxiedExternalCity.DoltMode, "proxied-server")
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
		{name: "explicit server", doltSection: "[dolt]\nmode = \"server\"\n", mode: "server", want: "server"},
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

// config.yaml mirrors a scope's persisted dolt_mode (persistedScopeDoltMode),
// including one from legacy metadata that canonicalization refuses as
// non-authoritative. Enforcement must not re-admit the refused mode through
// that mirror; the preserving startup path keeps it, so an existing scope is
// not migrated just by starting the city. configuredScopeDoltMode admits
// proxied-server without that refusal on desiredCityDoltConfigState's
// invariant that only endpoint-free configs carry the mirror: a refused
// proxied-server mirror lands on the fresh default anyway, and a city endpoint
// without an explicit proxied mode stays a direct server contract whatever the
// legacy metadata said.
func TestEnsureCanonicalScopeMetadataConfigMirrorOfRefusedLegacyMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		legacyMode string
		cityDolt   config.DoltConfig
		preserve   bool
		want       string
	}{
		{name: "enforce refuses the mirror", legacyMode: "embedded", want: "proxied-server"},
		{name: "preserve keeps the mirror", legacyMode: "embedded", preserve: true, want: "embedded"},
		{name: "enforce proxied mirror lands on the default", legacyMode: "proxied-server", want: "proxied-server"},
		{name: "enforce endpoint config carries no mirror", legacyMode: "proxied-server", cityDolt: config.DoltConfig{Host: "db.example.com", Port: 3306}, want: "server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			cityToml := "[workspace]\nname = \"demo\"\n"
			if tc.cityDolt.Host != "" {
				cityToml += "[dolt]\nhost = " + strconv.Quote(tc.cityDolt.Host) + "\nport = " + strconv.Itoa(tc.cityDolt.Port) + "\n"
			}
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
				t.Fatal(err)
			}
			metadata := `{"database":"legacy","backend":"legacy","dolt_mode":` + strconv.Quote(tc.legacyMode) + `,"dolt_database":"hq"}`
			if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
				t.Fatal(err)
			}
			state := desiredCityDoltConfigState(cityPath, tc.cityDolt, "gc")
			switch {
			case tc.cityDolt.Host != "" && state.EndpointOrigin != contract.EndpointOriginCityCanonical:
				t.Fatalf("precondition: desired city state = %+v, want a city_canonical endpoint", state)
			case tc.cityDolt.Host == "" && state.DoltMode != tc.legacyMode:
				t.Fatalf("precondition: config mirror dolt_mode = %q, want the legacy metadata's %s", state.DoltMode, tc.legacyMode)
			}
			if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, cityPath, state); err != nil {
				t.Fatalf("ensureCanonicalScopeConfigState: %v", err)
			}
			if err := ensureCanonicalScopeMetadata(fsys.OSFS{}, cityPath, "hq", tc.preserve); err != nil {
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

// An explicit proxied-server selection survives a city endpoint: the
// provider's local proxy fronts that endpoint (proxied-external). Metadata
// canonicalized before bd writes its own must carry the selected mode, or the
// two persisted markers split and the scope silently runs direct while
// config.yaml still selects the proxy. An endpoint configured with mode=server,
// or with no mode at all (the legacy shape), stays a direct server contract,
// and so does a scope-owned explicit endpoint whatever mode its config.yaml
// records: rig config has no transport selector, and desiredRigDoltConfigState
// writes every explicit endpoint as a direct server.
func TestEnsureCanonicalScopeMetadataKeepsConfiguredProxiedExternalMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		origin   contract.EndpointOrigin
		preserve bool
		want     string
	}{
		{name: "enforce proxied external", mode: "proxied-server", want: "proxied-server"},
		{name: "preserve proxied external", mode: "proxied-server", preserve: true, want: "proxied-server"},
		{name: "enforce explicit proxied external", mode: "proxied-server", origin: contract.EndpointOriginExplicit, want: "server"},
		{name: "preserve explicit proxied external", mode: "proxied-server", origin: contract.EndpointOriginExplicit, preserve: true, want: "server"},
		{name: "enforce direct external", mode: "server", want: "server"},
		{name: "preserve direct external", mode: "server", preserve: true, want: "server"},
		{name: "enforce legacy external without mode", want: "server"},
		{name: "preserve legacy external without mode", preserve: true, want: "server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cityDolt := config.DoltConfig{Host: "db.example.com", Port: 3306, Mode: tc.mode}
			cityToml := "[workspace]\nname = \"demo\"\n[dolt]\nhost = \"db.example.com\"\nport = 3306\n"
			if tc.mode != "" {
				cityToml += "mode = " + strconv.Quote(tc.mode) + "\n"
			}
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
				t.Fatal(err)
			}
			state := desiredCityDoltConfigState(cityPath, cityDolt, "gc")
			if state.EndpointOrigin != contract.EndpointOriginCityCanonical || state.DoltHost == "" || state.DoltPort == "" {
				t.Fatalf("precondition: desired city state = %+v, want a city_canonical endpoint", state)
			}
			if tc.origin != "" {
				state.EndpointOrigin = tc.origin
			}
			if tc.mode == "" {
				// Configs written before dolt.mode existed record the
				// endpoint alone.
				state.DoltMode = ""
			}
			if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, cityPath, state); err != nil {
				t.Fatalf("ensureCanonicalScopeConfigState: %v", err)
			}
			if err := ensureCanonicalScopeMetadata(fsys.OSFS{}, cityPath, "hq", tc.preserve); err != nil {
				t.Fatalf("ensureCanonicalScopeMetadata: %v", err)
			}
			mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "metadata.json"))
			if err != nil || !ok {
				t.Fatalf("ReadDoltMode: mode=%q ok=%v err=%v", mode, ok, err)
			}
			if mode != tc.want {
				t.Fatalf("metadata dolt_mode = %q, want %q", mode, tc.want)
			}
			if got, want := scopeUsesProxiedDoltMode(cityPath, cityPath), tc.want == "proxied-server"; got != want {
				t.Fatalf("scopeUsesProxiedDoltMode = %v, want %v for canonical metadata dolt_mode %q", got, want, mode)
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
