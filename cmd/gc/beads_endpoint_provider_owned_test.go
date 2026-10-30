package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writeBdOwnedDirectExternalScope builds the M3b shape: bd initialized the
// scope in direct (`dolt_mode: server`) mode against an external upstream, and
// bd's metadata.json is the only record of that upstream — gc leaves a
// provider-owned scope's config.yaml alone, so nothing else on disk knows the
// host and port.
func writeBdOwnedDirectExternalScope(t *testing.T, dir, doltDatabase string) {
	t.Helper()
	const (
		host = "db.example.com"
		port = "4406"
	)
	writeRigEndpointMetadata(t, dir, doltDatabase)
	path := filepath.Join(dir, ".beads", "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta["dolt_server_host"] = host
	meta["dolt_server_port"] = port
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeBdOwnedProxiedScope builds the M4 shape: bd's committed metadata binds
// the scope to the proxied-server path, which classifies as provider-owned with
// no ownership journal at all.
func writeBdOwnedProxiedScope(t *testing.T, dir, doltDatabase string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(dir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: doltDatabase,
	}); err != nil {
		t.Fatal(err)
	}
}

func readScopeMetadataMap(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func journalCityScopeOwnership(t *testing.T, cityPath, scopeRoot string, intent providerScopeIntent) {
	t.Helper()
	if err := persistProviderScopeOwnership(cityPath, scopeRoot, intent); err != nil {
		t.Fatalf("persist provider ownership: %v", err)
	}
	if err := markProviderScopeOwnershipReady(cityPath, scopeRoot); err != nil {
		t.Fatalf("mark provider ownership ready: %v", err)
	}
}

// TestBeadsCityEndpointRefusesProviderOwnedCity pins the ownership boundary the
// endpoint doors never asked about. `gc beads city use-managed`/`use-external`
// manage gc-owned endpoint topology; on a scope bd owns they would strip the
// only record of its upstream from metadata.json and write gc's endpoint keys
// into a config.yaml bd owns, leaving the city between two owners with no verb
// that repairs it.
func TestBeadsCityEndpointRefusesProviderOwnedCity(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, cityDir string)
		opts  cityEndpointOptions
	}{
		"use-managed on journaled direct-external city": {
			setup: func(t *testing.T, cityDir string) {
				writeBdOwnedDirectExternalScope(t, cityDir, "hosted")
				journalCityScopeOwnership(t, cityDir, cityDir, providerScopeIntent{Transport: "direct", Target: "external"})
			},
			opts: cityEndpointOptions{},
		},
		"use-external on journaled direct-external city": {
			setup: func(t *testing.T, cityDir string) {
				writeBdOwnedDirectExternalScope(t, cityDir, "hosted")
				journalCityScopeOwnership(t, cityDir, cityDir, providerScopeIntent{Transport: "direct", Target: "external"})
			},
			opts: cityEndpointOptions{External: true, Host: "other.example.com", Port: "4407", AdoptUnverified: true},
		},
		"use-external on proxied city": {
			setup: func(t *testing.T, cityDir string) {
				writeBdOwnedProxiedScope(t, cityDir, "hq")
			},
			opts: cityEndpointOptions{External: true, Host: "other.example.com", Port: "4407", AdoptUnverified: true},
		},
		"dry-run on proxied city": {
			setup: func(t *testing.T, cityDir string) {
				writeBdOwnedProxiedScope(t, cityDir, "hq")
			},
			opts: cityEndpointOptions{DryRun: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cityDir := t.TempDir()
			writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{}, nil)
			tc.setup(t, cityDir)
			t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))

			before := readScopeMetadataMap(t, cityDir)

			var stdout, stderr bytes.Buffer
			code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, tc.opts, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("doBeadsCityEndpoint() = %d, want 1 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "beads provider owns") {
				t.Fatalf("stderr = %q, want a provider-ownership refusal", stderr.String())
			}
			if after := readScopeMetadataMap(t, cityDir); !jsonEqual(t, before, after) {
				t.Fatalf("refused command rewrote bd's metadata: before=%v after=%v", before, after)
			}
			if _, err := os.Stat(filepath.Join(cityDir, ".beads", "config.yaml")); err == nil {
				t.Fatalf("refused command wrote gc endpoint keys into bd's config.yaml")
			}
		})
	}
}

// TestRigSetEndpointRefusesProviderOwnedRig is the same boundary through the
// rig door: `gc rig set-endpoint <rig> --inherit` on a bd-owned direct-external
// rig would erase the rig's only binding.
func TestRigSetEndpointRefusesProviderOwnedRig(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, cityDir, rigDir string)
		opts  rigEndpointOptions
	}{
		"inherit on journaled direct-external rig": {
			setup: func(t *testing.T, cityDir, rigDir string) { //nolint:revive // cityDir is used by the journaled variant
				writeBdOwnedDirectExternalScope(t, rigDir, "fe")
				journalCityScopeOwnership(t, cityDir, rigDir, providerScopeIntent{Transport: "direct", Target: "external"})
			},
			opts: rigEndpointOptions{Inherit: true},
		},
		"external on proxied rig": {
			setup: func(t *testing.T, cityDir, rigDir string) { //nolint:revive // cityDir is used by the journaled variant
				writeBdOwnedProxiedScope(t, rigDir, "fe")
			},
			opts: rigEndpointOptions{External: true, Host: "db.example.com", Port: "4406", AdoptUnverified: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cityDir := t.TempDir()
			rigDir := filepath.Join(cityDir, "rigs", "frontend")
			if err := os.MkdirAll(rigDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{}, []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}})
			writeRigEndpointMetadata(t, cityDir, "hq")
			tc.setup(t, cityDir, rigDir)
			t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))

			before := readScopeMetadataMap(t, rigDir)

			var stdout, stderr bytes.Buffer
			code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", tc.opts, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("doRigSetEndpoint() = %d, want 1 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "beads provider owns") {
				t.Fatalf("stderr = %q, want a provider-ownership refusal", stderr.String())
			}
			if after := readScopeMetadataMap(t, rigDir); !jsonEqual(t, before, after) {
				t.Fatalf("refused command rewrote bd's metadata: before=%v after=%v", before, after)
			}
		})
	}
}

// TestNoDoorErasesAProviderOwnedScopesUpstreamBinding closes the gap a
// per-command guard always has, exactly as TestNoDoorFlipsAnEmbeddedScopesStorageMode
// does for the storage mode. dolt_server_host/dolt_server_port are on
// contract's deprecatedMetadataKeys list and EnsureCanonicalMetadata deletes
// them unconditionally; for a bd-owned direct-external scope they are the only
// record of the upstream, so every canonicalizer reachable from an endpoint
// command is driven here.
func TestNoDoorErasesAProviderOwnedScopesUpstreamBinding(t *testing.T) {
	for name, canonicalize := range map[string]func(cityPath, scope string) error{
		"endpoint path, named scope": func(cityPath, scope string) error {
			return requireCanonicalizedScopeMetadata(fsys.OSFS{}, cityPath, scope)
		},
		"endpoint path, inherited rig": func(cityPath, scope string) error {
			return canonicalizeScopeMetadataIfPresent(fsys.OSFS{}, cityPath, scope)
		},
	} {
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			writeBdOwnedDirectExternalScope(t, city, "hosted")
			journalCityScopeOwnership(t, city, city, providerScopeIntent{Transport: "direct", Target: "external"})

			before := readScopeMetadataMap(t, city)
			err := canonicalize(city, city)
			after := readScopeMetadataMap(t, city)

			if err == nil {
				t.Fatalf("canonicalizer accepted a provider-owned scope")
			}
			if !strings.Contains(err.Error(), "beads provider owns") {
				t.Fatalf("canonicalize error = %v, want a provider-ownership refusal", err)
			}
			if !jsonEqual(t, before, after) {
				t.Fatalf("canonicalizer rewrote bd's metadata: before=%v after=%v", before, after)
			}
			if after["dolt_server_host"] != "db.example.com" || after["dolt_server_port"] != "4406" {
				t.Fatalf("upstream binding erased from metadata bd owns: %v", after)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b map[string]any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(left) == string(right)
}

// TestBootCanonicalizationKeepsBdsUpstreamBinding covers the door
// TestNoDoorErasesAProviderOwnedScopesUpstreamBinding could not reach. The
// endpoint doors refuse a provider-owned scope, but an un-journaled bd-owned
// direct-external city classifies as legacy-managed — the journal is runtime
// state under .gc, and regenerating it is enough to lose the record. Startup
// normalization then canonicalized bd's metadata and took dolt_server_host and
// dolt_server_port with it, silently re-homing the city onto a gc-managed Dolt
// with an empty store and no way back: after the rewrite the config says
// managed_city, so ResolveDoltConnectionTarget can never consult the binding
// again even if it were still there.
func TestBootCanonicalizationKeepsBdsUpstreamBinding(t *testing.T) {
	city := t.TempDir()
	writeBdOwnedDirectExternalCity(t, city)
	writeCityTOMLForBdProvider(t, city)

	owned, err := scopeProviderOwned(city, city)
	if err != nil {
		t.Fatalf("scopeProviderOwned: %v", err)
	}
	if owned {
		t.Skip("classifier now owns this shape; the boot door is guarded upstream")
	}

	if err := normalizeCanonicalBdScopeFilesForInit(city, city, "gc", ""); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}

	after := readScopeMetadataMap(t, city)
	if after["dolt_server_host"] != "db.example" {
		t.Fatalf("startup normalization erased bd's upstream host: %v", after)
	}
	if port, ok := after["dolt_server_port"].(float64); !ok || int(port) != 4406 {
		t.Fatalf("startup normalization erased bd's upstream port: %v", after)
	}
	// Keeping the record is only half the fix. The same pass used to stamp
	// `gc.endpoint_origin: managed_city` into bd's config.yaml, which is exactly
	// the marker that stops ResolveDoltConnectionTarget from ever consulting the
	// binding again — so the address survived and was unreachable.
	assertScopeConfigCarriesNoGCEndpointOrigin(t, city)
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget after the boot door: %v", err)
	}
	if target.Host != "db.example" || target.Port != "4406" {
		t.Fatalf("resolved target after the boot door = %q:%q, want db.example:4406", target.Host, target.Port)
	}
	if !target.External {
		t.Fatalf("resolved target after the boot door = %+v, want an external upstream", target)
	}
	// And gc must not raise its own Dolt over the city's empty `.beads/dolt`:
	// the store lives on db.example, so the managed lifecycle is not gc's.
	if owned, err := managedDoltLifecycleOwned(city); err != nil {
		t.Fatalf("managedDoltLifecycleOwned: %v", err)
	} else if owned {
		t.Fatalf("gc claims the managed Dolt lifecycle for a bd-owned direct-external city")
	}
}

// TestBootCanonicalizationKeepsBdsUpstreamRigBinding is the rig arm of
// TestBootCanonicalizationKeepsBdsUpstreamBinding. The door stamped
// `gc.endpoint_origin: inherited_city` into any rig the ownership classifier
// does not own, and the inherited-rig resolver treats that marker as proof the
// endpoint comes from the city gc manages — so a bd-owned direct-external rig
// on a gc-managed city resolved to gc's server, where its database does not
// exist, while `bd` in the same rig still reached db.example.
func TestBootCanonicalizationKeepsBdsUpstreamRigBinding(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "fe")
	rig := rigs["fe"]
	writeBdOwnedDirectExternalRig(t, rig, "fe", "db.example", 4406)
	// The state a real `gc start` is in when it classifies a rig:
	// startBeadsLifecycle raises the city's own provider, publishing
	// dolt-state.json, before it reaches the rig loop. Without this the
	// fixture tests a moment that never happens on disk, and a city-level
	// ownership signal would look safe here while re-homing every rig in
	// production.
	seedCityDatabaseDir(t, city, "hq")
	writeDoltRuntimePublicationFixture(t, city, managedDoltStatePath(city))

	owned, err := scopeProviderOwned(city, rig)
	if err != nil {
		t.Fatalf("scopeProviderOwned: %v", err)
	}
	if owned {
		t.Skip("classifier now owns this shape; the boot door is guarded upstream")
	}

	if err := normalizeCanonicalBdScopeFilesForInit(city, rig, "fe", ""); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}

	after := readScopeMetadataMap(t, rig)
	if after["dolt_server_host"] != "db.example" {
		t.Fatalf("startup normalization erased bd's upstream host: %v", after)
	}
	if port, ok := after["dolt_server_port"].(float64); !ok || int(port) != 4406 {
		t.Fatalf("startup normalization erased bd's upstream port: %v", after)
	}
	assertScopeConfigCarriesNoGCEndpointOrigin(t, rig)
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget after the boot door: %v", err)
	}
	if target.Host != "db.example" || target.Port != "4406" {
		t.Fatalf("resolved rig target after the boot door = %q:%q, want db.example:4406", target.Host, target.Port)
	}
	if !target.External {
		t.Fatalf("resolved rig target after the boot door = %+v, want an external upstream", target)
	}
}

// assertScopeConfigCarriesNoGCEndpointOrigin fails when the boot door wrote
// gc's endpoint-origin marker into a config.yaml bd owns. The marker is the
// discriminator every bd-owned fallback in contract rests on.
func assertScopeConfigCarriesNoGCEndpointOrigin(t *testing.T, scopeRoot string) {
	t.Helper()
	cfg, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadConfigState(%s): %v", scopeRoot, err)
	}
	if ok && cfg.EndpointOrigin != "" {
		t.Fatalf("boot door stamped gc.endpoint_origin %q into bd's config.yaml at %s", cfg.EndpointOrigin, scopeRoot)
	}
}

// writeBdOwnedDirectExternalRig replaces a rig's gc-canonical scope files with
// the shape bd leaves behind: bd's own config.yaml template and a metadata.json
// whose persisted server binding is the only record of the upstream.
func writeBdOwnedDirectExternalRig(t *testing.T, rigPath, prefix, host string, port int) {
	t.Helper()
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server",`+
		`"dolt_server_host":%q,"dolt_server_port":%d,"dolt_database":%q}`+"\n", host, port, prefix)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: "+prefix+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeCityTOMLForBdProvider gives a fixture city the minimum that makes
// cityUsesBdStoreContract true, so startup normalization actually runs.
func writeCityTOMLForBdProvider(t *testing.T, cityPath string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("name = \"fixture\"\n\n[beads]\nprovider = \"bd\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestScopeIsBdOwnedDirectExternal pins the predicate's evidence order. Its
// only discriminator between "bd's upstream" and "gc's own managed server" is
// the host recorded in bd's binding, compared against managedCityHost(), which
// reads GC_DOLT_HOST from the live environment. A gc-managed scope initialized
// under a non-loopback GC_DOLT_HOST records that host verbatim, so a later boot
// with the variable unset compares unequal and the scope reads as bd-owned: gc
// stops canonicalising it and stops raising its Dolt, and the beads stay
// unavailable until an operator restores the variable by hand.
//
// What settles it is per-scope evidence, and it takes two facts together — gc
// published a Dolt runtime for the city, AND the scope's own recorded database
// has a Dolt directory inside the city's managed data dir. Neither half decides
// alone, and the rig rows below are why: the publication is about the city, and
// by the time any rig is classified it always exists, because
// startBeadsLifecycle raises the city's provider before it reaches the rig
// loop.
func TestScopeIsBdOwnedDirectExternal(t *testing.T) {
	for name, tc := range map[string]struct {
		// setup prepares the fixture and returns the scope root to classify.
		setup func(t *testing.T, cityPath string) string
		want  bool
	}{
		// The M3b shape the predicate exists for: bd's own template config,
		// bd's binding naming a host that is not gc's, and no sign gc ever ran
		// a Dolt here.
		"bd's binding on a city gc never published a runtime for": {
			setup: func(t *testing.T, cityPath string) string {
				writeBdOwnedDirectExternalCity(t, cityPath)
				return cityPath
			},
			want: true,
		},
		"a published managed dolt runtime over the city's own database": {
			setup: func(t *testing.T, cityPath string) string {
				writeBdOwnedDirectExternalCity(t, cityPath)
				seedCityDatabaseDir(t, cityPath, "hosted")
				writeDoltRuntimePublicationFixture(t, cityPath, managedDoltStatePath(cityPath))
				return cityPath
			},
			want: false,
		},
		"a published provider dolt runtime over the city's own database": {
			setup: func(t *testing.T, cityPath string) string {
				writeBdOwnedDirectExternalCity(t, cityPath)
				seedCityDatabaseDir(t, cityPath, "hosted")
				writeDoltRuntimePublicationFixture(t, cityPath, providerManagedDoltStatePath(cityPath))
				return cityPath
			},
			want: false,
		},
		// The conjunction, from the city side: a publication proves gc raised
		// SOME Dolt here, not that this scope's beads are in it. A city whose
		// database is not under the managed data dir is still bd's.
		"a publication alone does not claim a city whose database is not gc's": {
			setup: func(t *testing.T, cityPath string) string {
				writeBdOwnedDirectExternalCity(t, cityPath)
				writeDoltRuntimePublicationFixture(t, cityPath, managedDoltStatePath(cityPath))
				return cityPath
			},
			want: true,
		},
		// The conjunction, from the rig side, and the regression this table
		// exists to stop: the city's own Dolt is up (it always is by the time
		// the rig loop runs) and its database sits in the managed data dir, but
		// the rig's beads are on a hosted server and it has no database there.
		// Classifying it as gc's would stamp `gc.endpoint_origin:
		// inherited_city` into bd's config.yaml and run `bd init` against gc's
		// server, leaving gc-native reads on an empty database while `bd` in
		// the rig still reached the real upstream.
		"a bd-owned rig keeps its upstream while the city's own dolt is up": {
			setup: func(t *testing.T, cityPath string) string {
				writeBdOwnedDirectExternalCity(t, cityPath)
				seedCityDatabaseDir(t, cityPath, "hosted")
				writeDoltRuntimePublicationFixture(t, cityPath, managedDoltStatePath(cityPath))
				rig := filepath.Join(cityPath, "rigs", "fe")
				writeBdOwnedDirectExternalRig(t, rig, "fe", "db.example", 4406)
				return rig
			},
			want: true,
		},
		// And the rig-flavored drift case the evidence closes: same
		// non-authoritative config and same recorded non-loopback host, but
		// this rig's database really is in the city's multi-database data dir,
		// which is the legacy gc-managed layout.
		"a rig whose database is in the city's data dir is gc's": {
			setup: func(t *testing.T, cityPath string) string {
				writeBdOwnedDirectExternalCity(t, cityPath)
				seedCityDatabaseDir(t, cityPath, "hosted")
				writeDoltRuntimePublicationFixture(t, cityPath, managedDoltStatePath(cityPath))
				rig := filepath.Join(cityPath, "rigs", "fe")
				writeBdOwnedDirectExternalRig(t, rig, "fe", "db.example", 4406)
				seedCityDatabaseDir(t, cityPath, "fe")
				return rig
			},
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			writeCityTOMLForBdProvider(t, city)
			scope := tc.setup(t, city)

			got, err := scopeIsBdOwnedDirectExternal(city, scope)
			if err != nil {
				t.Fatalf("scopeIsBdOwnedDirectExternal: %v", err)
			}
			if got != tc.want {
				t.Fatalf("scopeIsBdOwnedDirectExternal() = %v, want %v", got, tc.want)
			}
		})
	}
}

// writeDoltRuntimePublicationFixture writes a runtime state file at path. Only
// its existence is read: a state file left behind by a Dolt that died
// uncleanly, and the `running:false` one a clean `gc stop` leaves behind, are
// both still records that gc raised this city's Dolt. It is half the evidence
// scopeStoreLivesInTheCitysManagedDolt needs; the scope's own database
// directory is the other half.
func writeDoltRuntimePublicationFixture(t *testing.T, cityPath, statePath string) {
	t.Helper()
	if err := writeDoltRuntimeStateFile(statePath, doltRuntimeState{
		Running: true,
		PID:     os.Getpid(),
		Port:    3307,
		DataDir: filepath.Join(cityPath, ".beads", "dolt"),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(%s): %v", statePath, err)
	}
}
