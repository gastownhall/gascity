package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The on-disk shape is from the retained d3a native standalone adoption. The
// opaque sentinel is a filesystem fixture, not a simulated database pass.
func nativeDirectAdoptFixture(t *testing.T) (city, rig string, preserved map[string][]byte) {
	t.Helper()
	city, rig = t.TempDir(), filepath.Join(t.TempDir(), "existing-project")
	writeSchema2RigCity(t, city, "adopt-preserve", "[workspace]\n[beads]\nprovider=\"exec:"+gcBeadsBdScriptPath(city)+"\"\n", "")
	database := "gc_setup_3d575b641de741c6acf637cd76711805"
	files := map[string]string{
		".beads/metadata.json":   `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gc_setup_3d575b641de741c6acf637cd76711805","project_id":"dde7e826-2436-4159-b399-5344503f9a66"}`,
		".beads/config.yaml":     "# Native bd template\nissue-prefix: kept3d575b64\n",
		".beads/dolt-server.pid": "77985\n", ".beads/dolt-server.port": "63467\n",
		".beads/dolt/" + database + "/.dolt/noms/preservation-sentinel": "existing-native-store-bytes",
	}
	preserved = map[string][]byte{}
	for name, contents := range files {
		path := filepath.Join(rig, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		preserved[name] = []byte(contents)
	}
	return city, rig, preserved
}

func assertNativeDirectPreserved(t *testing.T, rig string, before map[string][]byte) {
	t.Helper()
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(rig, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("preserved %s = %q, err=%v; want exact original bytes", name, got, err)
		}
	}
}

func TestAdoptNativeDirectLocalKeepsProviderOwnership(t *testing.T) {
	city, rig, before := nativeDirectAdoptFixture(t)
	owned, err := scopeProviderOwned(city, rig)
	if err != nil || !owned {
		t.Fatalf("native standalone ownership=%v, err=%v; existing backend must remain provider-owned", owned, err)
	}
	if err := prepareRigAdoptProviderState(city, rig); err != nil {
		t.Fatal(err)
	}
	if err := normalizeCanonicalBdScopeFilesForInit(city, rig, "kept3d575b64", ""); err != nil {
		t.Fatal(err)
	}
	if err := syncConfiguredDoltPortFiles(city, config.DoltConfig{}, "gc", []config.Rig{{Name: "existing", Path: rig, Prefix: "kept3d575b64"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertNativeDirectPreserved(t, rig, before)
	if _, err := os.Stat(providerScopeOwnershipPath(city)); !os.IsNotExist(err) {
		t.Fatalf("classification invented an initialization journal: %v", err)
	}
}

func TestAdoptNativeDirectLocalReadEnvNeverFallsBackToCity(t *testing.T) {
	city, rig, before := nativeDirectAdoptFixture(t)
	for _, records := range []bool{true, false} {
		if !records {
			for _, name := range []string{"dolt-server.pid", "dolt-server.port"} {
				if err := os.Remove(filepath.Join(rig, ".beads", name)); err != nil {
					t.Fatal(err)
				}
			}
		}
		// Apply the actual native-rig read projection with deliberately different
		// city coordinates. Stopped local state cannot authorize another backend.
		env := map[string]string{"GC_DOLT_HOST": "city.example", "GC_DOLT_PORT": "43361", "BEADS_DOLT_SERVER_PORT": "43361", "GC_DOLT_DATA_DIR": "/foreign/city", "BEADS_DOLT_AUTO_START": "0", "BEADS_DIR": filepath.Join(rig, ".beads")}
		if err := applyResolvedRigDoltEnvContext(context.Background(), env, city, rig, nil, false); err != nil {
			t.Fatalf("records=%v: %v", records, err)
		}
		for _, key := range append(append([]string{}, projectedDoltEnvKeys...), "GC_DOLT_DATA_DIR", "BEADS_DOLT_AUTO_START") {
			if env[key] != "" {
				t.Fatalf("records=%v: city projection %s=%q reached native store", records, key, env[key])
			}
		}
		if env["BEADS_DIR"] != filepath.Join(rig, ".beads") {
			t.Fatal("native scope path changed")
		}
	}
	// Model only replacement of native ownership records after a restart. No
	// process is started; subsequent read projection must still stay scope-local.
	for name, value := range map[string]string{"dolt-server.pid": "88001\n", "dolt-server.port": "63468\n"} {
		if err := os.WriteFile(filepath.Join(rig, ".beads", name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env, err := bdRuntimeEnvForRigWithErrorNoRecovery(city, nil, rig)
	if err != nil {
		t.Fatal(err)
	}
	if env["BEADS_DIR"] != filepath.Join(rig, ".beads") || env["GC_DOLT_PORT"] != "" || env["BEADS_DOLT_SERVER_PORT"] != "" {
		t.Fatalf("restarted native projection=%v", env)
	}
	delete(before, ".beads/dolt-server.pid")
	delete(before, ".beads/dolt-server.port")
	assertNativeDirectPreserved(t, rig, before)
}

func TestAdoptNativeDirectLocalCustomTypesUseOriginalScope(t *testing.T) {
	city, rig, before := nativeDirectAdoptFixture(t)
	env, err := providerOwnedScopeCustomTypesEnv(city, rig)
	if err != nil {
		t.Fatal(err)
	}
	if env["BEADS_DIR"] != filepath.Join(rig, ".beads") {
		t.Fatalf("custom types scope=%q", env["BEADS_DIR"])
	}
	for _, key := range append(append([]string{}, projectedDoltEnvKeys...), "GC_DOLT_DATA_DIR", "BEADS_DOLT_AUTO_START") {
		if env[key] != "" {
			t.Fatalf("custom types inherited %s=%q", key, env[key])
		}
	}
	assertNativeDirectPreserved(t, rig, before)
}

func TestAdoptNativeDirectLocalRefusesUnprovenStorageBeforeMutation(t *testing.T) {
	for _, kind := range []string{"missing database", "unsafe database", "incomplete native records", "symlink database", "shared-server", "custom data-dir", "missing project identity", "partial host", "partial port", "local endpoint", "socket endpoint"} {
		t.Run(kind, func(t *testing.T) {
			city, rig, _ := nativeDirectAdoptFixture(t)
			database := filepath.Join(rig, ".beads/dolt/gc_setup_3d575b641de741c6acf637cd76711805")
			switch kind {
			case "missing database":
				if err := os.RemoveAll(database); err != nil {
					t.Fatal(err)
				}
			case "unsafe database":
				path := filepath.Join(rig, ".beads/metadata.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "gc_setup_3d575b641de741c6acf637cd76711805", "../foreign")), 0o600); err != nil {
					t.Fatal(err)
				}
			case "incomplete native records":
				if err := os.Remove(filepath.Join(rig, ".beads/dolt-server.port")); err != nil {
					t.Fatal(err)
				}
			case "symlink database":
				if err := os.Rename(database, database+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(database+"-retained", database); err != nil {
					t.Fatal(err)
				}
			case "missing project identity", "partial host", "partial port", "local endpoint", "socket endpoint":
				mutateNativeDirectIdentity(t, rig, kind)
			case "shared-server", "custom data-dir":
				setting := "dolt.shared-server: true\n"
				if kind == "custom data-dir" {
					setting = "dolt.data-dir: /foreign/data\n"
				}
				if err := os.WriteFile(filepath.Join(rig, ".beads/config.yaml"), []byte("issue-prefix: kept3d575b64\n"+setting), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := map[string][]byte{}
			for _, name := range []string{".beads/metadata.json", ".beads/config.yaml"} {
				bytes, err := os.ReadFile(filepath.Join(rig, name))
				if err != nil {
					t.Fatal(err)
				}
				before[name] = bytes
			}
			if err := prepareRigAdoptProviderState(city, rig); err == nil {
				t.Fatal("unproven existing server binding was allowed to enter initialization")
			}
			assertNativeDirectPreserved(t, rig, before)
		})
	}
}

func TestAdoptNativeDirectLocalDoesNotOverrideManagedOrForeignBindings(t *testing.T) {
	for _, kind := range []string{"managed canonical", "foreign metadata", "opaque binding", "embedded"} {
		t.Run(kind, func(t *testing.T) {
			city, rig, _ := nativeDirectAdoptFixture(t)
			switch kind {
			case "managed canonical":
				if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(rig, ".beads/config.yaml"), contract.ConfigState{IssuePrefix: "kept3d575b64", EndpointOrigin: contract.EndpointOriginExplicit, EndpointStatus: contract.EndpointStatusVerified, DoltHost: "127.0.0.1", DoltPort: "43361", DoltMode: "server"}); err != nil {
					t.Fatal(err)
				}
			case "opaque binding":
				path := filepath.Join(rig, ".beads/metadata.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				changed := strings.TrimSuffix(string(data), "}") + `,"storage_endpoint":"https://storage.example","storage_database":"foreign"}`
				if err = os.WriteFile(path, []byte(changed), 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign metadata", "embedded":
				path := filepath.Join(rig, ".beads/metadata.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				changed := strings.TrimSuffix(string(data), "}") + `,"dolt_server_host":"db.example","dolt_server_port":4406}`
				if kind == "embedded" {
					changed = strings.ReplaceAll(string(data), `"server"`, `"embedded"`)
				}
				if err = os.WriteFile(path, []byte(changed), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Query the actual existing classification surface, not the new helper.
			entry, owned, err := providerOwnedScopeState(city, rig)
			if err != nil {
				t.Fatal(err)
			}
			if owned {
				t.Fatalf("%s was relabeled as native local provider-owned: %+v", kind, entry)
			}
		})
	}
}

func TestAdoptNativeDirectLocalDoesNotAliasSameNamedCityDatabase(t *testing.T) {
	city, rig, _ := nativeDirectAdoptFixture(t)
	database := "gc_setup_3d575b641de741c6acf637cd76711805"
	if err := os.RemoveAll(filepath.Join(rig, ".beads/dolt", database)); err != nil {
		t.Fatal(err)
	}
	// An unrelated same-name directory on the city side is not a registration
	// or a migration proof for this external project.
	managedData := scopeDoltDataDir(city)
	if relative, err := filepath.Rel(city, managedData); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("test managed data escaped owned fixture: %s (%v)", managedData, err)
	}
	if err := os.MkdirAll(filepath.Join(managedData, database, ".dolt"), 0o700); err != nil {
		t.Fatal(err)
	}
	publication := providerManagedDoltStatePath(city)
	if err := os.MkdirAll(filepath.Dir(publication), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publication, []byte(fmt.Sprintf(`{"running":true,"pid":%d,"port":43361}`, os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareRigAdoptProviderState(city, rig); err == nil {
		t.Fatal("same-name city database was mistaken for this external store")
	}
}

// mutateNativeDirectIdentity retains native storage while making its binding
// ambiguous. Read paths must not turn missing identity into city authority.
func mutateNativeDirectIdentity(t *testing.T, rig, kind string) {
	t.Helper()
	path := filepath.Join(rig, ".beads/metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := string(data)
	switch kind {
	case "missing project identity":
		changed = strings.ReplaceAll(changed, `,"project_id":"dde7e826-2436-4159-b399-5344503f9a66"`, "")
	case "partial host":
		changed = strings.TrimSuffix(changed, "}") + `,"dolt_server_host":"foreign.example"}`
	case "partial port":
		changed = strings.TrimSuffix(changed, "}") + `,"dolt_server_port":4406}`
	case "local endpoint":
		changed = strings.TrimSuffix(changed, "}") + `,"dolt_server_host":"127.0.0.1","dolt_server_port":4406}`
	case "socket endpoint":
		changed = strings.TrimSuffix(changed, "}") + `,"dolt_server_socket":"/foreign/dolt.sock"}`
	default:
		t.Fatalf("unknown mutation %q", kind)
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptNativeDirectLocalMissingStorageRefusesRegisteredReads(t *testing.T) {
	for _, kind := range []string{"missing database", "shared-server", "custom data-dir", "missing project identity", "partial host", "partial port", "local endpoint", "socket endpoint"} {
		t.Run(kind, func(t *testing.T) {
			city, rig, _ := nativeDirectAdoptFixture(t)
			switch {
			case kind == "missing database":
				database := "gc_setup_3d575b641de741c6acf637cd76711805"
				if err := os.RemoveAll(filepath.Join(rig, ".beads/dolt", database)); err != nil {
					t.Fatal(err)
				}
			case kind != "shared-server" && kind != "custom data-dir":
				mutateNativeDirectIdentity(t, rig, kind)
			default:
				setting := "dolt.shared-server: true\n"
				if kind == "custom data-dir" {
					setting = "dolt.data-dir: /foreign/data\n"
				}
				if err := os.WriteFile(filepath.Join(rig, ".beads/config.yaml"), []byte("issue-prefix: kept3d575b64\n"+setting), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\n[beads]\nprovider=\"exec:"+gcBeadsBdScriptPath(city)+"\"\n[[rigs]]\nname=\"existing\"\npath=\""+rig+"\"\nprefix=\"kept3d575b64\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if env, err := bdRuntimeEnvForRigWithErrorNoRecovery(city, nil, rig); err == nil || env != nil {
				t.Fatalf("unproven local storage reached normal read projection: env=%v err=%v", env, err)
			}
			if env, err := providerOwnedScopeCustomTypesEnv(city, rig); err == nil || env != nil {
				t.Fatalf("unproven local storage reached custom types: env=%v err=%v", env, err)
			}
		})
	}
}

func TestAdoptNativeDirectLocalDoesNotReclassifyInitializingScope(t *testing.T) {
	city, rig, _ := nativeDirectAdoptFixture(t)
	if err := os.RemoveAll(filepath.Join(rig, ".beads/dolt")); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, rig, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	entry, owned, err := providerOwnedScopeState(city, rig)
	if err != nil || !owned || entry.State != providerScopeInitializing {
		t.Fatalf("pending initializer ownership changed: %+v %v %v", entry, owned, err)
	}
}

// An unregistered inherited-city scope carries explicit GC management
// authority. Admission must retain that existing contract without turning an
// unclaimed native store, invalid marker or wrong-scope marker into authority.
func TestAdoptNativeDirectLocalCanonicalManagedAdmissionRequiresValidatedAuthority(t *testing.T) {
	for _, kind := range []string{"inherited city authority", "unverified inherited city authority", "managed city marker on rig", "unknown verification status", "unclaimed missing identity"} {
		t.Run(kind, func(t *testing.T) {
			city, rig, before := nativeDirectAdoptFixture(t)
			wantErr := kind != "inherited city authority" && kind != "unverified inherited city authority" && kind != "unknown verification status"
			if kind == "unclaimed missing identity" {
				mutateNativeDirectIdentity(t, rig, "missing project identity")
			} else {
				origin := contract.EndpointOriginInheritedCity
				if kind == "managed city marker on rig" {
					origin = contract.EndpointOriginManagedCity
				}
				status := contract.EndpointStatusVerified
				if kind == "unverified inherited city authority" {
					status = contract.EndpointStatusUnverified
				}
				configPath := filepath.Join(rig, ".beads", "config.yaml")
				if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, configPath, contract.ConfigState{
					IssuePrefix: "kept3d575b64", EndpointOrigin: origin,
					EndpointStatus: status, DoltMode: "server",
				}); err != nil {
					t.Fatal(err)
				}
				// The existing contract derives unknown verification status from
				// retained authority; connectivity is still checked by init.
				if kind == "unknown verification status" {
					data, err := os.ReadFile(configPath)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Count(string(data), "gc.endpoint_status: verified") != 1 {
						t.Fatal("fixture lost its canonical status marker")
					}
					if err := os.WriteFile(configPath, []byte(strings.Replace(string(data), "gc.endpoint_status: verified", "gc.endpoint_status: invalid", 1)), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			for name := range before {
				data, err := os.ReadFile(filepath.Join(rig, name))
				if err != nil {
					t.Fatal(err)
				}
				before[name] = data
			}
			err := prepareRigAdoptProviderState(city, rig)
			if (err != nil) != wantErr {
				t.Fatalf("canonical admission error=%v, want refusal=%v", err, wantErr)
			}
			assertNativeDirectPreserved(t, rig, before)
			if _, err := os.Stat(providerScopeOwnershipPath(city)); !os.IsNotExist(err) {
				t.Fatalf("admission invented an initialization journal: %v", err)
			}
			if !wantErr {
				if _, owned, err := providerOwnedScopeState(city, rig); err != nil || owned {
					t.Fatalf("canonical GC authority was relabeled provider-owned: %v %v", owned, err)
				}
			}
		})
	}
}
