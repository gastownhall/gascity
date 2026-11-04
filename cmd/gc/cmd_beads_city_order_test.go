package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// endpointWriteRecordingFS records attempted writes, so rollback cannot hide
// that a classification failure happened after mutating the old topology.
type endpointWriteRecordingFS struct {
	fsys.FS
	writes []string
}

func (f *endpointWriteRecordingFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	f.writes = append(f.writes, path)
	return f.FS.WriteFile(path, data, mode)
}

func writeEndpointOrderScope(t *testing.T, root, prefix string, state contract.ConfigState) {
	t.Helper()
	writeRigEndpointMetadata(t, root, prefix)
	writeMetadataProjectID(t, filepath.Join(root, ".beads", "metadata.json"), "endpoint-order-"+prefix)
	state.IssuePrefix = prefix
	state.DoltMode = "server"
	writeRigEndpointCanonicalConfig(t, root, state)
}

func endpointOrderFixture(t *testing.T, managed bool) (string, string) {
	t.Helper()
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "frontend")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, city, config.DoltConfig{}, []config.Rig{{Name: "frontend", Path: rig, Prefix: "fe"}})
	cityState := contract.ConfigState{EndpointOrigin: contract.EndpointOriginCityCanonical, EndpointStatus: contract.EndpointStatusVerified, DoltHost: "old.example.com", DoltPort: "3306"}
	if managed {
		cityState = contract.ConfigState{EndpointOrigin: contract.EndpointOriginManagedCity, EndpointStatus: contract.EndpointStatusVerified}
	}
	writeEndpointOrderScope(t, city, "hq", cityState)
	rigState := cityState
	rigState.EndpointOrigin = contract.EndpointOriginInheritedCity
	writeEndpointOrderScope(t, rig, "fe", rigState)
	// The explicit exec provider is a bounded no-op test seam, not a Dolt
	// process. No endpoint verification or native database is required.
	script := writeManagedBdTestScript(t, "#!/bin/sh\nexit 0\n")
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", city)
	return city, rig
}

func TestCityEndpointMigrationClassifiesUnchangedTopology(t *testing.T) {
	for _, managed := range []bool{true, false} {
		name := "external_to_managed"
		if managed {
			name = "managed_to_external"
		}
		t.Run(name, func(t *testing.T) {
			city, rig := endpointOrderFixture(t, managed)
			options := cityEndpointOptions{}
			if managed {
				options = cityEndpointOptions{External: true, Host: "new.example.com", Port: "4406", AdoptUnverified: true}
			}
			var stdout, stderr bytes.Buffer
			if code := doBeadsCityEndpoint(fsys.OSFS{}, city, options, &stdout, &stderr); code != 0 {
				t.Fatalf("migration refused valid original topology: exit=%d stderr=%s", code, stderr.String())
			}
			cityState := readRigEndpointConfigState(t, city)
			rigState := readRigEndpointConfigState(t, rig)
			if rigState.EndpointOrigin != contract.EndpointOriginInheritedCity || rigState.DoltHost != cityState.DoltHost || rigState.DoltPort != cityState.DoltPort {
				t.Fatalf("migration did not publish matching city/rig endpoints: city=%+v rig=%+v", cityState, rigState)
			}
			if managed && cityState.EndpointOrigin != contract.EndpointOriginCityCanonical || !managed && cityState.EndpointOrigin != contract.EndpointOriginManagedCity {
				t.Fatalf("migration reached wrong target: %+v", cityState)
			}
		})
	}
}

func TestCityEndpointLateOwnershipRefusalPrecedesEveryWrite(t *testing.T) {
	city, _ := endpointOrderFixture(t, false)
	unknown := filepath.Join(city, "rigs", "unknown")
	writeRigEndpointMetadata(t, unknown, "unknown")
	// This later rig deliberately has no project or gc authority: it must
	// refuse, rather than treating the proposed managed endpoint as proof.
	writeRigEndpointCanonicalConfig(t, unknown, contract.ConfigState{IssuePrefix: "unknown"})
	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(city, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "unknown", Path: unknown, Prefix: "unknown"})
	if err := writeCityConfigForEditFS(fsys.OSFS{}, filepath.Join(city, "city.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	fs := &endpointWriteRecordingFS{FS: fsys.OSFS{}}
	var stdout, stderr bytes.Buffer
	if code := doBeadsCityEndpoint(fs, city, cityEndpointOptions{}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "no proven project identity") {
		t.Fatalf("unknown later ownership must refuse specifically: exit=%d stderr=%s", code, stderr.String())
	}
	if len(fs.writes) != 0 {
		t.Fatalf("classification refusal happened after writes (rollback is not preflight): %v", fs.writes)
	}
}

func TestCityEndpointMigrationPreservesAllProviderOwnedArtifacts(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(map[bool]string{true: "dry_run", false: "transition"}[dryRun], func(t *testing.T) {
			city, _ := endpointOrderFixture(t, false)
			owned := filepath.Join(city, "rigs", "owned")
			writeBdOwnedProxiedScope(t, owned, "owned")
			writeMetadataProjectID(t, filepath.Join(owned, ".beads", "metadata.json"), "bd-owned-project")
			writeRigEndpointCanonicalConfig(t, owned, contract.ConfigState{IssuePrefix: "owned"})
			cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(city, "city.toml"))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Rigs = []config.Rig{{Name: "owned", Path: owned, Prefix: "owned", DoltHost: "bd-upstream.example.com", DoltPort: "4501"}}
			if err := writeCityConfigForEditFS(fsys.OSFS{}, filepath.Join(city, "city.toml"), cfg); err != nil {
				t.Fatal(err)
			}
			journalCityScopeOwnership(t, city, owned, providerScopeIntent{Transport: "proxied", Target: "local"})
			for name, body := range map[string]string{"dolt-server.pid": "314159\n", "dolt-server.port": "42424\n"} {
				if err := os.WriteFile(filepath.Join(owned, ".beads", name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := map[string][]byte{}
			for _, name := range []string{"metadata.json", "config.yaml", "dolt-server.pid", "dolt-server.port"} {
				before[name] = mustReadFile(t, filepath.Join(owned, ".beads", name))
			}
			fs := &endpointWriteRecordingFS{FS: fsys.OSFS{}}
			var stdout, stderr bytes.Buffer
			if code := doBeadsCityEndpoint(fs, city, cityEndpointOptions{DryRun: dryRun}, &stdout, &stderr); code != 0 {
				t.Fatalf("city endpoint transition failed: exit=%d stderr=%s", code, stderr.String())
			}
			for name, data := range before {
				if got := mustReadFile(t, filepath.Join(owned, ".beads", name)); !bytes.Equal(got, data) {
					t.Fatalf("provider-owned %s changed: before=%q after=%q", name, data, got)
				}
			}
			for _, path := range fs.writes {
				if strings.HasPrefix(path, owned+string(filepath.Separator)) {
					t.Fatalf("attempted provider-owned write: %s", path)
				}
			}
			after := readCityEndpointRigCompat(t, city, "owned")
			if after.DoltHost != "bd-upstream.example.com" || after.DoltPort != "4501" {
				t.Fatalf("provider-owned compat binding changed: %+v", after)
			}
		})
	}
}

// Endpoint validation can yield to another actor after ownership planning. The
// old-topology result must not authorize rewriting changed scope identity.
func TestCityEndpointPlannedMetadataDriftPrecedesEveryWrite(t *testing.T) {
	city, rig := endpointOrderFixture(t, true)
	original := verifyCityExternalEndpoint
	t.Cleanup(func() { verifyCityExternalEndpoint = original })
	verifyCityExternalEndpoint = func(_ contract.ConfigState, scopeRoot, _ string) error {
		if scopeRoot == city {
			writeMetadataProjectID(t, filepath.Join(rig, ".beads", "metadata.json"), "changed-after-planning")
		}
		return nil
	}
	fs := &endpointWriteRecordingFS{FS: fsys.OSFS{}}
	var stdout, stderr bytes.Buffer
	options := cityEndpointOptions{External: true, Host: "new.example.com", Port: "4406"}
	if code := doBeadsCityEndpoint(fs, city, options, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "files changed after ownership classification") {
		t.Fatalf("changed identity must refuse specifically: exit=%d stderr=%s", code, stderr.String())
	}
	if len(fs.writes) != 0 {
		t.Fatalf("changed identity refusal attempted writes: %v", fs.writes)
	}
	if data := mustReadFile(t, filepath.Join(rig, ".beads", "metadata.json")); !bytes.Contains(data, []byte("changed-after-planning")) {
		t.Fatalf("concurrent metadata was overwritten: %s", data)
	}
}
