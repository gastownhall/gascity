package main

import (
	"bytes"
	"encoding/json"
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
	writeBdOwnedDirectExternalCity(t, city, "db.example", "4406")
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
}

// writeCityTOMLForBdProvider gives a fixture city the minimum that makes
// cityUsesBdStoreContract true, so startup normalization actually runs.
func writeCityTOMLForBdProvider(t *testing.T, cityPath string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("name = \"fixture\"\n\n[beads]\nprovider = \"bd\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
