package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	beadsbackend "github.com/steveyegge/beads/backend"
)

// registerLibraryBackendForTest registers name with the linked beads library
// for the duration of the test, standing in for a build's distribution wiring
// (the enterprise build's bdhttp.Register). A name this binary's own wiring
// already registered is left alone: the library panics on a duplicate, and the
// distribution's registration is then the one under test.
func registerLibraryBackendForTest(t *testing.T, name string) {
	t.Helper()
	if beadsbackend.Registered(name) {
		return
	}
	refuse := func(context.Context, string) (beadsbackend.DoltStorage, error) {
		return nil, errors.New("test backend does not open")
	}
	beadsbackend.Register(name, beadsbackend.Backend{Open: refuse, OpenReadOnly: refuse})
	t.Cleanup(func() { beadsbackend.Deregister(name) })
}

const (
	// convertedHTTPMetadata is the metadata.json `bd connect <url>
	// --convert-workspace` writes: the backend, the database it names on the
	// server, and the project it expects — no dolt_mode, no storage fields.
	convertedHTTPMetadata = `{"database":"dolt","backend":"http","dolt_database":"hq","project_id":"proj-1"}`
	// convertedHTTPTarget is the http_target.json the same conversion writes
	// beside it.
	convertedHTTPTarget = `{"url":"https://beads.example.test","expect_project_id":"proj-1","api":"v0"}`
)

// writeLibraryBackendScope registers the http backend and writes an
// authoritative scope in the shape `bd connect <url> --convert-workspace`
// leaves: metadata names the backend and carries no storage-binding fields,
// because the target lives beside it in http_target.json.
func writeLibraryBackendScope(t *testing.T) string {
	t.Helper()
	registerLibraryBackendForTest(t, "http")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"
	for name, body := range map[string]string{
		"config.yaml":      config,
		"metadata.json":    convertedHTTPMetadata,
		"http_target.json": convertedHTTPTarget,
	} {
		if err := os.WriteFile(filepath.Join(cityPath, ".beads", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cityPath
}

// writeInheritingRig writes a rig whose scope config inherits the city's
// endpoint and which carries no metadata of its own.
func writeInheritingRig(t *testing.T) string {
	t.Helper()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "issue_prefix: frontend\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return rigPath
}

func TestScopeHasCompleteStorageBindingRecognizesLibraryExtensionBackend(t *testing.T) {
	if !beadsbackend.Registered("http") {
		cityPath := t.TempDir()
		path := scopeMetadataJSONPath(cityPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(convertedHTTPMetadata), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := scopeHasCompleteStorageBinding(path)
		if err != nil || got {
			t.Fatalf("before registration scopeHasCompleteStorageBinding() = (%v, %v), want (false, nil)", got, err)
		}
	}

	cityPath := writeLibraryBackendScope(t)
	got, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(cityPath))
	if err != nil || !got {
		t.Fatalf("scopeHasCompleteStorageBinding() = (%v, %v), want (true, nil) for a library extension backend", got, err)
	}
}

// A backend the library registered but that is off the self-sufficient
// allowlist is not an opaque binding when it carries no storage fields: gc
// keeps classifying it exactly as it did before the library learned the name.
func TestScopeHasCompleteStorageBindingIgnoresLibraryBackendOffTheAllowlist(t *testing.T) {
	const name = "gctest-ext"
	registerLibraryBackendForTest(t, name)
	cityPath := t.TempDir()
	path := scopeMetadataJSONPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"database":"dolt","backend":"`+name+`","dolt_database":"hq"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := scopeHasCompleteStorageBinding(path)
	if err != nil || got {
		t.Fatalf("scopeHasCompleteStorageBinding() = (%v, %v), want (false, nil) for a backend off the allowlist", got, err)
	}
}

// A rig that inherits its city's endpoint inherits the city's library backend
// with it: gc does not own that rig's store either.
func TestScopeStoreIsExternallyBoundInheritsALibraryBackendCity(t *testing.T) {
	cityPath := writeLibraryBackendScope(t)
	rigPath := writeInheritingRig(t)

	for _, scope := range []string{cityPath, rigPath} {
		bound, err := scopeStoreIsExternallyBound(cityPath, scope)
		if err != nil || !bound {
			t.Fatalf("scopeStoreIsExternallyBound(%s) = (%v, %v), want (true, nil)", scope, bound, err)
		}
	}
}

// The canonicalization guards leave a converted workspace alone: gc must not
// stamp its Dolt vocabulary over metadata the library reads, nor touch the
// target file beside it, for the city or for a rig that inherits it.
func TestCanonicalizationGuardsSkipALibraryBackendScope(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeLibraryBackendScope(t)
	rigPath := writeInheritingRig(t)

	for _, scope := range []string{cityPath, rigPath} {
		skips, err := scopeSkipsManagedDoltForInit(cityPath, scope)
		if err != nil || !skips {
			t.Fatalf("scopeSkipsManagedDoltForInit(%s) = (%v, %v), want (true, nil)", scope, skips, err)
		}
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	if err := normalizeCanonicalBdScopeFiles(cityPath, cfg, io.Discard); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFiles: %v", err)
	}
	for name, want := range map[string]string{"metadata.json": convertedHTTPMetadata, "http_target.json": convertedHTTPTarget} {
		if got := string(mustReadFile(t, filepath.Join(cityPath, ".beads", name))); got != want {
			t.Fatalf("%s changed by normalization:\n got %s\nwant %s", name, got, want)
		}
	}
}

// City start skips managed Dolt for a converted workspace: the provider never
// runs and nothing in .beads is rewritten.
func TestStartBeadsLifecycleSkipsManagedDoltForALibraryBackendCity(t *testing.T) {
	cityPath := writeLibraryBackendScope(t)
	callLog := filepath.Join(cityPath, "provider-calls.log")
	script := writeManagedBdTestScript(t, "#!/bin/sh\necho \"$1\" >> "+callLog+"\nexit 99\n")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	if err := healthBeadsProviderContext(context.Background(), cityPath, false); err != nil {
		t.Fatalf("healthBeadsProviderContext: %v", err)
	}
	if err := startBeadsLifecycle(cityPath, "demo", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	if _, err := os.Stat(callLog); !os.IsNotExist(err) {
		t.Fatalf("managed provider ran for a library backend city, stat err = %v", err)
	}
	for name, want := range map[string]string{"metadata.json": convertedHTTPMetadata, "http_target.json": convertedHTTPTarget} {
		if got := string(mustReadFile(t, filepath.Join(cityPath, ".beads", name))); got != want {
			t.Fatalf("%s changed at city start:\n got %s\nwant %s", name, got, want)
		}
	}
}

// A registered extension does not loosen the partial-binding refusal: storage
// fields that are present must still be complete.
func TestScopeHasCompleteStorageBindingPartialFieldsFailClosedForLibraryExtension(t *testing.T) {
	const name = "http"
	registerLibraryBackendForTest(t, name)
	cityPath := t.TempDir()
	path := scopeMetadataJSONPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"backend":"`+name+`","storage_endpoint":"remote"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := scopeHasCompleteStorageBinding(path); err == nil {
		t.Fatal("scopeHasCompleteStorageBinding() accepted a partial binding")
	}
}

// gc projects no backend environment for a library extension backend: bd
// reads the workspace and its own credentials, so every projected backend key
// is withheld rather than pointing bd at a Dolt endpoint.
func TestLibraryExtensionBackendWithholdsProjectedBackendEnv(t *testing.T) {
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	for _, tc := range []struct {
		name  string
		apply func(env map[string]string, cityPath string) (bool, error)
	}{
		{name: "scope", apply: func(env map[string]string, cityPath string) (bool, error) {
			return applyCanonicalScopeBackendEnv(env, cityPath, cityPath)
		}},
		{name: "city", apply: applyCityStorageBindingEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := writeLibraryBackendScope(t)
			env := map[string]string{"GC_DOLT_HOST": "leftover.example.test", "BEADS_DOLT_SERVER_PORT": "3307"}
			used, err := tc.apply(env, cityPath)
			if err != nil || !used {
				t.Fatalf("projection = (%v, %v), want (true, nil)", used, err)
			}
			for _, key := range []string{
				"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT",
				"GC_BEADS_BACKEND", "BEADS_BACKEND",
			} {
				if got, ok := env[key]; !ok || got != "" {
					t.Errorf("env[%q] = (%q, present=%v), want it withheld as empty", key, got, ok)
				}
			}
		})
	}
}
